package graphreader

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

const defaultMaxGraphReaderCacheBytes = 256 * 1024

type CachedInspector struct {
	inner    Inspector
	redis    *redis.Client
	ttl      time.Duration
	logger   *slog.Logger
	maxBytes int
}

func NewCached(inner Inspector, redisClient *redis.Client, ttl time.Duration, logger *slog.Logger) Inspector {
	return NewCachedWithMaxBytes(inner, redisClient, ttl, defaultMaxGraphReaderCacheBytes, logger)
}

func NewCachedWithMaxBytes(inner Inspector, redisClient *redis.Client, ttl time.Duration, maxBytes int, logger *slog.Logger) Inspector {
	if inner == nil || redisClient == nil || ttl <= 0 {
		return inner
	}
	if logger == nil {
		logger = slog.Default()
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxGraphReaderCacheBytes
	}
	return &CachedInspector{
		inner:    inner,
		redis:    redisClient,
		ttl:      ttl,
		logger:   logger.With("component", "graphreader-cache"),
		maxBytes: maxBytes,
	}
}

func (c *CachedInspector) Inspect(ctx context.Context, params InspectParams) (InspectResult, error) {
	if c == nil || c.inner == nil {
		return InspectResult{}, fmt.Errorf("graphreader cache: inner inspector is not configured")
	}
	key, err := graphReaderCacheKey(params)
	if err == nil && c.redis != nil {
		if raw, getErr := c.redis.Get(ctx, key).Bytes(); getErr == nil {
			var cached InspectResult
			decodeErr := json.Unmarshal(raw, &cached)
			if decodeErr == nil {
				return cached, nil
			}
			c.logger.Warn("graph cache decode failed", "err", decodeErr.Error())
		} else if getErr != redis.Nil {
			c.logger.Warn("graph cache read failed", "err", getErr.Error())
		}
	}

	result, inspectErr := c.inner.Inspect(ctx, params)
	if inspectErr != nil {
		return result, inspectErr
	}
	if err == nil && c.redis != nil {
		if raw, encodeErr := json.Marshal(result); encodeErr == nil {
			if len(raw) > c.maxBytes {
				c.logger.Warn("graph cache payload skipped", "bytes", len(raw), "maxBytes", c.maxBytes)
				return result, nil
			}
			if setErr := c.redis.Set(ctx, key, raw, c.ttl).Err(); setErr != nil {
				c.logger.Warn("graph cache write failed", "err", setErr.Error())
			}
		} else {
			c.logger.Warn("graph cache encode failed", "err", encodeErr.Error())
		}
	}
	return result, nil
}

func graphReaderCacheKey(params InspectParams) (string, error) {
	type cacheScope struct {
		WorkspaceID    string `json:"workspaceId"`
		RepoID         int32  `json:"repoId"`
		Revision       string `json:"revision"`
		CommitHash     string `json:"commitHash"`
		SchemaVersion  int32  `json:"schemaVersion"`
		BuilderVersion string `json:"builderVersion"`
	}
	type cacheSeed struct {
		WorkspaceID string `json:"workspaceId"`
		NodeVID     string `json:"nodeVid"`
	}
	type cachePayload struct {
		OrgID         int32        `json:"orgId"`
		Query         string       `json:"query"`
		WorkspaceIDs  []string     `json:"workspaceIds"`
		Seeds         []cacheSeed  `json:"seeds"`
		AllowedScopes []cacheScope `json:"allowedScopes"`
		Limit         int32        `json:"limit"`
		MaxDepth      int          `json:"maxDepth"`
		Strict        bool         `json:"strict"`
		MaxSeedTokens int          `json:"maxSeedTokens"`
		SeedRowLimit  int32        `json:"seedRowLimit"`
		SeedVIDLimit  int32        `json:"seedVidLimit"`
		TraversalRows int32        `json:"traversalRows"`
	}

	workspaceIDs := cleanUnique(params.WorkspaceIDs)
	sort.Strings(workspaceIDs)

	seeds := make([]cacheSeed, 0, len(params.Seeds))
	for _, seed := range params.Seeds {
		if seed.WorkspaceID == "" || seed.NodeVID == "" {
			continue
		}
		seeds = append(seeds, cacheSeed{WorkspaceID: seed.WorkspaceID, NodeVID: seed.NodeVID})
	}
	sort.Slice(seeds, func(i, j int) bool {
		if seeds[i].WorkspaceID != seeds[j].WorkspaceID {
			return seeds[i].WorkspaceID < seeds[j].WorkspaceID
		}
		return seeds[i].NodeVID < seeds[j].NodeVID
	})

	scopes := make([]cacheScope, 0, len(params.AllowedScopes))
	for _, scope := range params.AllowedScopes {
		scopes = append(scopes, cacheScope{
			WorkspaceID:    scope.WorkspaceID,
			RepoID:         scope.RepoID,
			Revision:       scope.Revision,
			CommitHash:     scope.CommitHash,
			SchemaVersion:  scope.SchemaVersion,
			BuilderVersion: scope.BuilderVersion,
		})
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].WorkspaceID != scopes[j].WorkspaceID {
			return scopes[i].WorkspaceID < scopes[j].WorkspaceID
		}
		if scopes[i].RepoID != scopes[j].RepoID {
			return scopes[i].RepoID < scopes[j].RepoID
		}
		if scopes[i].Revision != scopes[j].Revision {
			return scopes[i].Revision < scopes[j].Revision
		}
		if scopes[i].CommitHash != scopes[j].CommitHash {
			return scopes[i].CommitHash < scopes[j].CommitHash
		}
		if scopes[i].SchemaVersion != scopes[j].SchemaVersion {
			return scopes[i].SchemaVersion < scopes[j].SchemaVersion
		}
		return scopes[i].BuilderVersion < scopes[j].BuilderVersion
	})

	raw, err := json.Marshal(cachePayload{
		OrgID:         params.OrgID,
		Query:         params.Query,
		WorkspaceIDs:  workspaceIDs,
		Seeds:         seeds,
		AllowedScopes: scopes,
		Limit:         params.Limit,
		MaxDepth:      params.MaxDepth,
		Strict:        params.Strict,
		MaxSeedTokens: params.MaxSeedTokens,
		SeedRowLimit:  params.SeedRowLimit,
		SeedVIDLimit:  params.SeedVIDLimit,
		TraversalRows: params.TraversalRows,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "codeintel:graphreader:v2:" + fmt.Sprintf("%x", sum[:]), nil
}
