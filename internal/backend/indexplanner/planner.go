// Package indexplanner expands one user-visible repo indexing
// job into durable layer subjobs for the hot/cold executor
// fabric.
package indexplanner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"codeintel/internal/backend/indexsubjobs"
	"codeintel/internal/backend/workerclasses"
)

type SCIPProject struct {
	Language         string
	ProjectRoot      string
	Indexer          string
	SCIPWorkerClass  string
	ToolchainDigest  *string
	ImageDigest      *string
	ProjectInputHash *string
}

type Revision struct {
	WorkspaceID *string
	Branch      string
	Revision    string
	CommitHash  string

	RunClone         bool
	RunZoekt         bool
	RunASTTreeSitter bool
	RunGraphMerge    bool
	RunActivate      bool

	SCIPProjects []SCIPProject
}

type Input struct {
	RepoIndexingJobID string
	OrgID             int32
	RepoID            int32
	MaxAttempts       int32
	Revisions         []Revision
}

type subjobWriter interface {
	UpsertQueued(context.Context, indexsubjobs.CreateInput) error
}

var ErrInvalidPlan = errors.New("indexplanner: invalid plan")

func Build(in Input) ([]indexsubjobs.CreateInput, error) {
	if in.RepoIndexingJobID == "" || in.OrgID <= 0 || in.RepoID <= 0 || len(in.Revisions) == 0 {
		return nil, ErrInvalidPlan
	}
	core, ok := workerclasses.ByName("core")
	if !ok {
		return nil, fmt.Errorf("%w: core worker class is not registered", ErrInvalidPlan)
	}

	var out []indexsubjobs.CreateInput
	for _, rev := range in.Revisions {
		if rev.Branch == "" || rev.Revision == "" || rev.CommitHash == "" {
			return nil, ErrInvalidPlan
		}
		if rev.WorkspaceID == nil || *rev.WorkspaceID == "" {
			return nil, ErrInvalidPlan
		}
		if rev.RunClone {
			return nil, fmt.Errorf("%w: clone is a repo-wide layer and is not revision-planned", ErrInvalidPlan)
		}
		hasGraphProducer := rev.RunASTTreeSitter || len(rev.SCIPProjects) > 0
		hasProducer := rev.RunZoekt || hasGraphProducer
		if rev.RunGraphMerge && !hasGraphProducer {
			return nil, fmt.Errorf("%w: graph merge requires AST/tree-sitter or SCIP producer work", ErrInvalidPlan)
		}
		if rev.RunActivate && !hasProducer && !rev.RunGraphMerge {
			return nil, fmt.Errorf("%w: activate requires at least one producer layer", ErrInvalidPlan)
		}
		if rev.RunZoekt {
			out = append(out, coreSubjob(in, rev, indexsubjobs.LayerZoekt, core))
		}
		if rev.RunASTTreeSitter {
			out = append(out, coreSubjob(in, rev, indexsubjobs.LayerASTTreeSitter, core))
		}
		for _, project := range rev.SCIPProjects {
			subjob, err := scipSubjob(in, rev, project)
			if err != nil {
				return nil, err
			}
			out = append(out, subjob)
		}
		if rev.RunGraphMerge {
			out = append(out, coreSubjob(in, rev, indexsubjobs.LayerGraphMerge, core))
		}
		if rev.RunActivate {
			out = append(out, coreSubjob(in, rev, indexsubjobs.LayerActivate, core))
		}
	}
	if len(out) == 0 {
		return nil, ErrInvalidPlan
	}
	return out, nil
}

func PlanAndPersist(ctx context.Context, store subjobWriter, in Input) ([]indexsubjobs.CreateInput, error) {
	if store == nil {
		return nil, errors.New("indexplanner: store is required")
	}
	subjobs, err := Build(in)
	if err != nil {
		return nil, err
	}
	for _, subjob := range subjobs {
		if err := store.UpsertQueued(ctx, subjob); err != nil {
			return nil, err
		}
	}
	return subjobs, nil
}

func coreSubjob(in Input, rev Revision, layer indexsubjobs.Layer, class workerclasses.WorkerClass) indexsubjobs.CreateInput {
	inputDigest := digest("input", in.RepoIndexingJobID, stringPtrValue(rev.WorkspaceID), rev.Branch, rev.Revision, rev.CommitHash, string(layer), class.Name)
	return indexsubjobs.CreateInput{
		ID:                stableID(in, rev, layer, class.Name, "", "", ""),
		RepoIndexingJobID: in.RepoIndexingJobID,
		OrgID:             in.OrgID,
		WorkspaceID:       rev.WorkspaceID,
		RepoID:            in.RepoID,
		Branch:            rev.Branch,
		Revision:          rev.Revision,
		CommitHash:        rev.CommitHash,
		Layer:             layer,
		WorkerClass:       class.Name,
		QueueName:         class.QueueName,
		MaxAttempts:       in.MaxAttempts,
		InputDigest:       &inputDigest,
	}
}

func scipSubjob(in Input, rev Revision, project SCIPProject) (indexsubjobs.CreateInput, error) {
	if project.Language == "" || project.Indexer == "" || project.SCIPWorkerClass == "" {
		return indexsubjobs.CreateInput{}, ErrInvalidPlan
	}
	class, ok := workerclasses.ForSCIPWorkerClass(project.SCIPWorkerClass)
	if !ok {
		return indexsubjobs.CreateInput{}, fmt.Errorf("%w: no executor class for SCIP workerClass %q", ErrInvalidPlan, project.SCIPWorkerClass)
	}
	language := project.Language
	projectRoot, ok := NormalizeProjectRoot(project.ProjectRoot)
	if !ok {
		return indexsubjobs.CreateInput{}, fmt.Errorf("%w: projectRoot %q is not repo-relative", ErrInvalidPlan, project.ProjectRoot)
	}
	indexer := project.Indexer
	inputDigest := digest("input", in.RepoIndexingJobID, stringPtrValue(rev.WorkspaceID), rev.Branch, rev.Revision, rev.CommitHash, language, projectRoot, indexer, project.SCIPWorkerClass)
	if project.ProjectInputHash != nil && *project.ProjectInputHash != "" {
		inputDigest = *project.ProjectInputHash
	}
	return indexsubjobs.CreateInput{
		ID:                stableID(in, rev, indexsubjobs.LayerSCIP, class.Name, language, projectRoot, indexer),
		RepoIndexingJobID: in.RepoIndexingJobID,
		OrgID:             in.OrgID,
		WorkspaceID:       rev.WorkspaceID,
		RepoID:            in.RepoID,
		Branch:            rev.Branch,
		Revision:          rev.Revision,
		CommitHash:        rev.CommitHash,
		Layer:             indexsubjobs.LayerSCIP,
		Language:          &language,
		ProjectRoot:       &projectRoot,
		Indexer:           &indexer,
		WorkerClass:       class.Name,
		QueueName:         class.QueueName,
		MaxAttempts:       in.MaxAttempts,
		InputDigest:       &inputDigest,
		ToolchainDigest:   project.ToolchainDigest,
		ImageDigest:       project.ImageDigest,
	}, nil
}

func stableID(in Input, rev Revision, layer indexsubjobs.Layer, workerClass, language, projectRoot, indexer string) string {
	sum := digest(
		"subjob",
		in.RepoIndexingJobID,
		fmt.Sprint(in.OrgID),
		stringPtrValue(rev.WorkspaceID),
		fmt.Sprint(in.RepoID),
		rev.Branch,
		rev.Revision,
		rev.CommitHash,
		string(layer),
		workerClass,
		language,
		projectRoot,
		indexer,
	)
	return "isj_" + sum[:32]
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TrimProjectRoot(root string) string {
	normalized, _ := NormalizeProjectRoot(root)
	return normalized
}

func NormalizeProjectRoot(root string) (string, bool) {
	root = strings.TrimSpace(root)
	if root == "." {
		return "", true
	}
	if root == "" {
		return "", true
	}
	if strings.HasPrefix(root, "/") || strings.HasPrefix(root, `\`) || strings.Contains(root, `\`) {
		return "", false
	}
	clean := path.Clean(root)
	if clean == "." || clean == "" {
		return "", true
	}
	if clean != root || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", false
	}
	return strings.Trim(clean, "/"), true
}
