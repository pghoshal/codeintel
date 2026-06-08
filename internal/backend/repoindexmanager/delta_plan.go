package repoindexmanager

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	zoektStrategyNoop            = "NOOP"
	zoektStrategyDeltaFiles      = "DELTA_FILES"
	zoektStrategyFullRepo        = "FULL_REPO"
	zoektStrategyFullRepoRewrite = "FULL_REPO_REWRITE"
)

type deltaReindexPlan struct {
	Mode           string            `json:"mode"`
	AddedFiles     []string          `json:"addedFiles"`
	ChangedFiles   []string          `json:"changedFiles"`
	DeletedFiles   []string          `json:"deletedFiles"`
	UnchangedFiles []string          `json:"unchangedFiles"`
	Zoekt          deltaZoektPlan    `json:"zoekt"`
	SCIP           deltaSCIPPlan     `json:"scip"`
	Graph          deltaGraphPlan    `json:"graph"`
	Semantic       deltaSemanticPlan `json:"semantic"`
}

type deltaZoektPlan struct {
	Strategy string   `json:"strategy"`
	Files    []string `json:"files"`
	Reason   *string  `json:"reason,omitempty"`
}

type deltaSCIPPlan struct {
	Strategy     string   `json:"strategy"`
	ProjectRoots []string `json:"projectRoots"`
	Files        []string `json:"files"`
	Reason       *string  `json:"reason,omitempty"`
}

type deltaGraphPlan struct {
	Strategy     string   `json:"strategy"`
	Files        []string `json:"files"`
	DeletedFiles []string `json:"deletedFiles"`
}

type deltaSemanticPlan struct {
	Strategy string   `json:"strategy"`
	Files    []string `json:"files"`
	Reason   *string  `json:"reason,omitempty"`
}

func forceSemanticRepairPlan(plan *deltaReindexPlan, current []manifestFileRow, reason string) *deltaReindexPlan {
	currentByPath := manifestFileMap(current)
	allFiles := sortedManifestKeys(currentByPath)
	allRoots := uniqueManifestProjectRoots(current)
	reasonPtr := stringPtrValue(reason)
	if plan == nil {
		plan = &deltaReindexPlan{}
	}
	copyPlan := *plan
	copyPlan.Mode = "SEMANTIC_REPAIR"
	copyPlan.Zoekt = deltaZoektPlan{Strategy: "NOOP", Files: nil, Reason: nil}
	copyPlan.SCIP = deltaSCIPPlan{Strategy: "FULL_REPO", ProjectRoots: allRoots, Files: allFiles, Reason: reasonPtr}
	copyPlan.Graph = deltaGraphPlan{Strategy: "DELTA_FILES", Files: allFiles, DeletedFiles: nil}
	copyPlan.Semantic = deltaSemanticPlan{Strategy: "SKIPPED", Files: nil, Reason: reasonPtr}
	return &copyPlan
}

func manifestHasSemanticCandidates(files []manifestFileRow) bool {
	for _, file := range files {
		language := ""
		if file.Language != nil {
			language = strings.ToLower(strings.TrimSpace(*file.Language))
		}
		if language == "" {
			language = inferSemanticLanguageFromPath(file.Path)
		}
		switch language {
		case "typescript", "javascript", "go", "python", "java", "cpp", "c", "c++", "dotnet", "csharp", "ruby", "rust", "dart":
			return true
		}
	}
	return false
}

func inferSemanticLanguageFromPath(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".ts"), strings.HasSuffix(lower, ".tsx"), strings.HasSuffix(lower, ".js"), strings.HasSuffix(lower, ".jsx"), strings.HasSuffix(lower, ".mts"), strings.HasSuffix(lower, ".cts"), strings.HasSuffix(lower, ".mjs"), strings.HasSuffix(lower, ".cjs"):
		return "typescript"
	case strings.HasSuffix(lower, ".go"):
		return "go"
	case strings.HasSuffix(lower, ".py"):
		return "python"
	case strings.HasSuffix(lower, ".java"), strings.HasSuffix(lower, ".scala"), strings.HasSuffix(lower, ".kt"):
		return "java"
	case strings.HasSuffix(lower, ".cc"), strings.HasSuffix(lower, ".cpp"), strings.HasSuffix(lower, ".cxx"), strings.HasSuffix(lower, ".c"), strings.HasSuffix(lower, ".h"), strings.HasSuffix(lower, ".hpp"):
		return "cpp"
	case strings.HasSuffix(lower, ".cs"), strings.HasSuffix(lower, ".vb"), strings.HasSuffix(lower, ".fs"):
		return "dotnet"
	case strings.HasSuffix(lower, ".rb"):
		return "ruby"
	case strings.HasSuffix(lower, ".rs"):
		return "rust"
	case strings.HasSuffix(lower, ".dart"):
		return "dart"
	default:
		return ""
	}
}

func buildManifestDeltaPlan(previous, current []manifestFileRow, supportsZoektFileDelta bool) *deltaReindexPlan {
	currentByPath := manifestFileMap(current)
	if len(previous) == 0 {
		allFiles := sortedManifestKeys(currentByPath)
		allRoots := uniqueManifestProjectRoots(current)
		noPrevious := stringPtrValue("No previous index manifest exists.")
		return &deltaReindexPlan{
			Mode:           "FULL",
			AddedFiles:     allFiles,
			ChangedFiles:   nil,
			DeletedFiles:   nil,
			UnchangedFiles: nil,
			Zoekt:          deltaZoektPlan{Strategy: zoektStrategyFullRepo, Files: allFiles, Reason: noPrevious},
			SCIP:           deltaSCIPPlan{Strategy: "FULL_REPO", ProjectRoots: allRoots, Files: allFiles, Reason: noPrevious},
			Graph:          deltaGraphPlan{Strategy: "DELTA_FILES", Files: allFiles, DeletedFiles: nil},
			Semantic:       deltaSemanticPlan{Strategy: "SKIPPED", Files: nil},
		}
	}

	previousByPath := manifestFileMap(previous)
	var added, changed, deleted, unchanged []string
	for path, curr := range currentByPath {
		prev, ok := previousByPath[path]
		switch {
		case !ok:
			added = append(added, path)
		case prev.ContentHash != curr.ContentHash:
			changed = append(changed, path)
		default:
			unchanged = append(unchanged, path)
		}
	}
	for path := range previousByPath {
		if _, ok := currentByPath[path]; !ok {
			deleted = append(deleted, path)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(deleted)
	sort.Strings(unchanged)

	touched := append(append([]string{}, added...), changed...)
	mode := "DELTA"
	if len(added) == 0 && len(changed) == 0 && len(deleted) == 0 {
		mode = "NOOP"
	}
	zoektStrategy := zoektStrategyFullRepoRewrite
	zoektFiles := touched
	zoektReason := stringPtrValue("Zoekt rewrites repository shard files because file-level shard delta is not enabled.")
	if mode == "NOOP" {
		zoektStrategy = zoektStrategyNoop
		zoektFiles = nil
		zoektReason = nil
	} else if supportsZoektFileDelta {
		zoektStrategy = zoektStrategyDeltaFiles
		zoektReason = nil
	}
	scipStrategy := "PROJECT_ROOTS"
	scipRoots := uniqueManifestProjectRootsForPaths(currentByPath, touched)
	scipFiles := touched
	if mode == "NOOP" {
		scipStrategy = "NOOP"
		scipFiles = nil
		scipRoots = nil
	}
	graphStrategy := "DELTA_FILES"
	graphFiles := touched
	if mode == "NOOP" {
		graphStrategy = "NOOP"
		graphFiles = nil
	}
	return &deltaReindexPlan{
		Mode:           mode,
		AddedFiles:     added,
		ChangedFiles:   changed,
		DeletedFiles:   deleted,
		UnchangedFiles: unchanged,
		Zoekt:          deltaZoektPlan{Strategy: zoektStrategy, Files: zoektFiles, Reason: zoektReason},
		SCIP:           deltaSCIPPlan{Strategy: scipStrategy, ProjectRoots: scipRoots, Files: scipFiles},
		Graph:          deltaGraphPlan{Strategy: graphStrategy, Files: graphFiles, DeletedFiles: deleted},
		Semantic:       deltaSemanticPlan{Strategy: "SKIPPED", Files: nil},
	}
}

func manifestPlanValues(plan *deltaReindexPlan) (any, int32, int32, int32, int32) {
	if plan == nil {
		return nil, 0, 0, 0, 0
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, int32(len(plan.AddedFiles)), int32(len(plan.ChangedFiles)), int32(len(plan.DeletedFiles)), int32(len(plan.UnchangedFiles))
	}
	return string(raw), int32(len(plan.AddedFiles)), int32(len(plan.ChangedFiles)), int32(len(plan.DeletedFiles)), int32(len(plan.UnchangedFiles))
}

func manifestFileMap(files []manifestFileRow) map[string]manifestFileRow {
	out := make(map[string]manifestFileRow, len(files))
	for _, file := range files {
		if file.Path != "" {
			out[file.Path] = file
		}
	}
	return out
}

func sortedManifestKeys(values map[string]manifestFileRow) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func uniqueManifestProjectRoots(files []manifestFileRow) []string {
	seen := map[string]bool{}
	var out []string
	for _, file := range files {
		root := "."
		if file.ProjectRoot != nil && *file.ProjectRoot != "" {
			root = *file.ProjectRoot
		}
		if !seen[root] {
			seen[root] = true
			out = append(out, root)
		}
	}
	sort.Strings(out)
	return out
}

func uniqueManifestProjectRootsForPaths(files map[string]manifestFileRow, paths []string) []string {
	var selected []manifestFileRow
	for _, path := range paths {
		if file, ok := files[path]; ok {
			selected = append(selected, file)
		}
	}
	return uniqueManifestProjectRoots(selected)
}

func stringPtrValue(value string) *string {
	return &value
}

func (p *deltaReindexPlan) validate() error {
	if p == nil {
		return fmt.Errorf("nil delta plan")
	}
	if p.Mode == "" || p.Zoekt.Strategy == "" || p.SCIP.Strategy == "" || p.Graph.Strategy == "" || p.Semantic.Strategy == "" {
		return fmt.Errorf("delta plan has empty strategy")
	}
	switch p.Zoekt.Strategy {
	case zoektStrategyNoop, zoektStrategyDeltaFiles, zoektStrategyFullRepo, zoektStrategyFullRepoRewrite:
	default:
		return fmt.Errorf("delta plan has unsupported zoekt strategy %q", p.Zoekt.Strategy)
	}
	return nil
}
