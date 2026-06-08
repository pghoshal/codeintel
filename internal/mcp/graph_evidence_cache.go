package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"codeintel/internal/db"
	"codeintel/internal/graphreader"

	"github.com/redis/go-redis/v9"
)

const (
	defaultMaxGraphInspectionCacheBytes  = 256 * 1024
	defaultMaxCodegraphContextCacheBytes = 256 * 1024
)

type GraphEvidenceCache interface {
	GetGraphInspection(ctx context.Context, key string) (cachedGraphInspection, bool, error)
	SetGraphInspection(ctx context.Context, key string, value cachedGraphInspection, ttl time.Duration) error
	GetCodegraphContext(ctx context.Context, key string) (toolResult, bool, error)
	SetCodegraphContext(ctx context.Context, key string, value toolResult, ttl time.Duration) error
}

type RedisGraphEvidenceCache struct {
	redis                    *redis.Client
	logger                   *slog.Logger
	maxGraphInspectionBytes  int
	maxCodegraphContextBytes int
}

func NewRedisGraphEvidenceCache(redisClient *redis.Client, logger *slog.Logger) *RedisGraphEvidenceCache {
	return NewRedisGraphEvidenceCacheWithLimits(redisClient, logger, defaultMaxGraphInspectionCacheBytes, defaultMaxCodegraphContextCacheBytes)
}

func NewRedisGraphEvidenceCacheWithLimits(redisClient *redis.Client, logger *slog.Logger, maxGraphInspectionBytes, maxCodegraphContextBytes int) *RedisGraphEvidenceCache {
	if redisClient == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if maxGraphInspectionBytes <= 0 {
		maxGraphInspectionBytes = defaultMaxGraphInspectionCacheBytes
	}
	if maxCodegraphContextBytes <= 0 {
		maxCodegraphContextBytes = defaultMaxCodegraphContextCacheBytes
	}
	return &RedisGraphEvidenceCache{
		redis:                    redisClient,
		logger:                   logger.With("component", "mcp-graph-evidence-cache"),
		maxGraphInspectionBytes:  maxGraphInspectionBytes,
		maxCodegraphContextBytes: maxCodegraphContextBytes,
	}
}

func (c *RedisGraphEvidenceCache) GetGraphInspection(ctx context.Context, key string) (cachedGraphInspection, bool, error) {
	if c == nil || c.redis == nil || key == "" {
		return cachedGraphInspection{}, false, nil
	}
	raw, err := c.redis.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return cachedGraphInspection{}, false, nil
	}
	if err != nil {
		return cachedGraphInspection{}, false, err
	}
	var out cachedGraphInspection
	if err := json.Unmarshal(raw, &out); err != nil {
		return cachedGraphInspection{}, false, err
	}
	return out, true, nil
}

func (c *RedisGraphEvidenceCache) SetGraphInspection(ctx context.Context, key string, value cachedGraphInspection, ttl time.Duration) error {
	if c == nil || c.redis == nil || key == "" || ttl <= 0 {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > c.maxGraphInspectionBytes {
		c.logger.Warn("graph inspection cache payload skipped", "bytes", len(raw), "maxBytes", c.maxGraphInspectionBytes)
		return nil
	}
	return c.redis.Set(ctx, key, raw, ttl).Err()
}

func (c *RedisGraphEvidenceCache) GetCodegraphContext(ctx context.Context, key string) (toolResult, bool, error) {
	if c == nil || c.redis == nil || key == "" {
		return toolResult{}, false, nil
	}
	raw, err := c.redis.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return toolResult{}, false, nil
	}
	if err != nil {
		return toolResult{}, false, err
	}
	var out toolResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return toolResult{}, false, err
	}
	return out, true, nil
}

func (c *RedisGraphEvidenceCache) SetCodegraphContext(ctx context.Context, key string, value toolResult, ttl time.Duration) error {
	if c == nil || c.redis == nil || key == "" || ttl <= 0 {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > c.maxCodegraphContextBytes {
		c.logger.Warn("codegraph context cache payload skipped", "bytes", len(raw), "maxBytes", c.maxCodegraphContextBytes)
		return nil
	}
	return c.redis.Set(ctx, key, raw, ttl).Err()
}

type cachedGraphInspection struct {
	Output      string                         `json:"output"`
	Evidence    db.CodeGraphInspectionEvidence `json:"evidence"`
	GraphResult graphreader.InspectResult      `json:"graphReader"`
}

func graphInspectionToolResult(cached cachedGraphInspection) toolResult {
	return toolResult{
		Content: []toolContent{{Type: "text", Text: cached.Output}},
		Meta: map[string]any{
			"mode":        "active-code-graph",
			"evidence":    cached.Evidence,
			"graphReader": cached.GraphResult,
			"cache":       "hit",
		},
	}
}

type graphInspectionCacheKeyParams struct {
	OrgID         int32
	Query         string
	Repos         []string
	Revisions     []string
	Scopes        []db.RepoRevisionScope
	ActiveScopes  []db.CodeGraphActiveScope
	Limit         int32
	MaxDepth      int
	Compact       bool
	MaxSeedTokens int
	SeedRowLimit  int32
	SeedVIDLimit  int32
	TraversalRows int32
}

func graphInspectionCacheKey(p graphInspectionCacheKeyParams) (string, error) {
	type payloadScope struct {
		Repo               string   `json:"repo"`
		RevisionCandidates []string `json:"revisionCandidates"`
	}
	type payloadActiveScope struct {
		GraphIndexID   string `json:"graphIndexId"`
		RepoID         int32  `json:"repoId"`
		Revision       string `json:"revision"`
		CommitHash     string `json:"commitHash"`
		WorkspaceID    string `json:"workspaceId"`
		SchemaVersion  int32  `json:"schemaVersion"`
		BuilderVersion string `json:"builderVersion"`
	}
	type payload struct {
		OrgID         int32                `json:"orgId"`
		Query         string               `json:"query"`
		Repos         []string             `json:"repos"`
		Revisions     []string             `json:"revisions"`
		Scopes        []payloadScope       `json:"scopes"`
		ActiveScopes  []payloadActiveScope `json:"activeScopes"`
		Limit         int32                `json:"limit"`
		MaxDepth      int                  `json:"maxDepth"`
		Compact       bool                 `json:"compact"`
		MaxSeedTokens int                  `json:"maxSeedTokens"`
		SeedRowLimit  int32                `json:"seedRowLimit"`
		SeedVIDLimit  int32                `json:"seedVidLimit"`
		TraversalRows int32                `json:"traversalRows"`
	}

	repos := cleanStrings(p.Repos)
	sort.Strings(repos)
	revisions := cleanStrings(p.Revisions)
	sort.Strings(revisions)

	scopes := make([]payloadScope, 0, len(p.Scopes))
	for _, scope := range p.Scopes {
		revisionCandidates := cleanStrings(scope.RevisionCandidates)
		sort.Strings(revisionCandidates)
		scopes = append(scopes, payloadScope{
			Repo:               scope.Repo,
			RevisionCandidates: revisionCandidates,
		})
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Repo != scopes[j].Repo {
			return scopes[i].Repo < scopes[j].Repo
		}
		return stringsJoinForKey(scopes[i].RevisionCandidates) < stringsJoinForKey(scopes[j].RevisionCandidates)
	})

	active := make([]payloadActiveScope, 0, len(p.ActiveScopes))
	for _, scope := range p.ActiveScopes {
		active = append(active, payloadActiveScope{
			GraphIndexID:   scope.GraphIndexID,
			RepoID:         scope.RepoID,
			Revision:       scope.Revision,
			CommitHash:     scope.CommitHash,
			WorkspaceID:    scope.WorkspaceID,
			SchemaVersion:  scope.SchemaVersion,
			BuilderVersion: scope.BuilderVersion,
		})
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].RepoID != active[j].RepoID {
			return active[i].RepoID < active[j].RepoID
		}
		if active[i].Revision != active[j].Revision {
			return active[i].Revision < active[j].Revision
		}
		if active[i].CommitHash != active[j].CommitHash {
			return active[i].CommitHash < active[j].CommitHash
		}
		return active[i].GraphIndexID < active[j].GraphIndexID
	})

	raw, err := json.Marshal(payload{
		OrgID:         p.OrgID,
		Query:         p.Query,
		Repos:         repos,
		Revisions:     revisions,
		Scopes:        scopes,
		ActiveScopes:  active,
		Limit:         p.Limit,
		MaxDepth:      p.MaxDepth,
		Compact:       p.Compact,
		MaxSeedTokens: p.MaxSeedTokens,
		SeedRowLimit:  p.SeedRowLimit,
		SeedVIDLimit:  p.SeedVIDLimit,
		TraversalRows: p.TraversalRows,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "codeintel:mcp:graph-inspection:v2:" + fmt.Sprintf("%x", sum[:]), nil
}

type codegraphContextCacheKeyParams struct {
	OrgID        int32
	Query        string
	Repos        []string
	RequestedRef string
	Limit        int32
	Depth        *int32
	Compact      bool
	ActiveScopes []db.CodeGraphActiveScope
}

func codegraphContextCacheKey(p codegraphContextCacheKeyParams) (string, error) {
	type payloadActiveScope struct {
		GraphIndexID   string `json:"graphIndexId"`
		RepoID         int32  `json:"repoId"`
		Revision       string `json:"revision"`
		CommitHash     string `json:"commitHash"`
		WorkspaceID    string `json:"workspaceId"`
		SchemaVersion  int32  `json:"schemaVersion"`
		BuilderVersion string `json:"builderVersion"`
	}
	type payload struct {
		OrgID        int32                `json:"orgId"`
		Query        string               `json:"query"`
		Repos        []string             `json:"repos"`
		RequestedRef string               `json:"requestedRef"`
		Limit        int32                `json:"limit"`
		Depth        *int32               `json:"depth,omitempty"`
		Compact      bool                 `json:"compact"`
		ActiveScopes []payloadActiveScope `json:"activeScopes"`
	}
	repos := cleanStrings(p.Repos)
	sort.Strings(repos)
	active := make([]payloadActiveScope, 0, len(p.ActiveScopes))
	for _, scope := range p.ActiveScopes {
		active = append(active, payloadActiveScope{
			GraphIndexID:   scope.GraphIndexID,
			RepoID:         scope.RepoID,
			Revision:       scope.Revision,
			CommitHash:     scope.CommitHash,
			WorkspaceID:    scope.WorkspaceID,
			SchemaVersion:  scope.SchemaVersion,
			BuilderVersion: scope.BuilderVersion,
		})
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].RepoID != active[j].RepoID {
			return active[i].RepoID < active[j].RepoID
		}
		if active[i].Revision != active[j].Revision {
			return active[i].Revision < active[j].Revision
		}
		if active[i].CommitHash != active[j].CommitHash {
			return active[i].CommitHash < active[j].CommitHash
		}
		return active[i].GraphIndexID < active[j].GraphIndexID
	})
	raw, err := json.Marshal(payload{
		OrgID:        p.OrgID,
		Query:        p.Query,
		Repos:        repos,
		RequestedRef: p.RequestedRef,
		Limit:        p.Limit,
		Depth:        p.Depth,
		Compact:      p.Compact,
		ActiveScopes: active,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "codeintel:mcp:codegraph-context:v5:" + fmt.Sprintf("%x", sum[:]), nil
}

func stringsJoinForKey(values []string) string {
	if len(values) == 0 {
		return ""
	}
	out, _ := json.Marshal(values)
	return string(out)
}
