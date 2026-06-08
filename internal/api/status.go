// GET /api/status — org-level status rollup. Returns four top-
// level blocks: org identity, repo counts + indexing-job
// aggregate, connection counts + sync-job aggregate, and Zoekt
// search-backend metadata.
//
// Response shape:
//
//	{
//	  "org":  {"id": <int>, "name": "<string>", "domain": "<string>"},
//	  "repos": {
//	    "total":   <int>,
//	    "indexed": <int>,
//	    "indexingJobs": {
//	      "pending":    <int>,
//	      "inProgress": <int>,
//	      "failed":     <int>,
//	      "recentFailures": [
//	        {
//	          "id":           "<cuid>",
//	          "repo":         {"id": <int>, "name": "<string>"},
//	          "errorMessage": "<string>" | null,
//	          "createdAt":    "<iso>",
//	          "updatedAt":    "<iso>"
//	        }
//	      ]
//	    }
//	  },
//	  "connections": {
//	    "total":  <int>,
//	    "synced": <int>,
//	    "syncJobs": {
//	      "pending":    <int>,
//	      "inProgress": <int>,
//	      "failed":     <int>,
//	      "recentFailures": [
//	        {
//	          "id":            "<cuid>",
//	          "connection":    {"id": <int>, "name": "<string>", "connectionType": "<string>"},
//	          "errorMessage":  "<string>" | null,
//	          "createdAt":     "<iso>",
//	          "updatedAt":     "<iso>"
//	        }
//	      ]
//	    }
//	  },
//	  "zoekt": {
//	    "mode":        "single" | "fanout" | "routed" | "org-directory",
//	    "orgIndex":    null | {…},
//	    "shardGroups": [{…}, …],
//	    "endpoints":   [{…}, …]
//	  }
//	}
//
// Authenticated reads only (any non-guest org member). The
// `zoekt.orgIndex` / `shardGroups` / `endpoints` are emitted as
// `null` / `[]` / `[]` until the Zoekt-side persistence and
// health-probe surfaces are wired; `mode` is resolved from
// Config.ZoektWebserverUrls so the wire shape is byte-correct for
// deployments without a configured Zoekt backend.
package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/pkg/repopaths"
	"golang.org/x/sync/errgroup"
)

// recentFailureLimit caps how many recent FAILED jobs appear in
// each rollup branch (connection sync + repo indexing).
const recentFailureLimit = 5

type statusOrg struct {
	ID     int32  `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

// repoIndexFailureRepo is the nested {id, name} block per
// recent failed repo-indexing row.
type repoIndexFailureRepo struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// repoIndexFailureItem field order matches the documented wire
// projection: id, repo, errorMessage, createdAt, updatedAt.
type repoIndexFailureItem struct {
	ID           string               `json:"id"`
	Repo         repoIndexFailureRepo `json:"repo"`
	ErrorMessage *string              `json:"errorMessage"`
	CreatedAt    iso8601MilliTime     `json:"createdAt"`
	UpdatedAt    iso8601MilliTime     `json:"updatedAt"`
}

type statusRepoIndexingJobs struct {
	Pending        int32                  `json:"pending"`
	InProgress     int32                  `json:"inProgress"`
	Failed         int32                  `json:"failed"`
	RecentFailures []repoIndexFailureItem `json:"recentFailures"`
}

type statusRepos struct {
	Total        int32                  `json:"total"`
	Indexed      int32                  `json:"indexed"`
	IndexingJobs statusRepoIndexingJobs `json:"indexingJobs"`
}

// recentFailureConnection mirrors the nested {id, name,
// connectionType} block included on each connection-sync-failure row.
type recentFailureConnection struct {
	ID             int32  `json:"id"`
	Name           string `json:"name"`
	ConnectionType string `json:"connectionType"`
}

// recentFailureItem field order matches the wire projection:
// id, connection, errorMessage, createdAt, updatedAt.
type recentFailureItem struct {
	ID           string                  `json:"id"`
	Connection   recentFailureConnection `json:"connection"`
	ErrorMessage *string                 `json:"errorMessage"`
	CreatedAt    iso8601MilliTime        `json:"createdAt"`
	UpdatedAt    iso8601MilliTime        `json:"updatedAt"`
}

type statusSyncJobs struct {
	Pending        int32               `json:"pending"`
	InProgress     int32               `json:"inProgress"`
	Failed         int32               `json:"failed"`
	RecentFailures []recentFailureItem `json:"recentFailures"`
}

type statusConnections struct {
	Total    int32          `json:"total"`
	Synced   int32          `json:"synced"`
	SyncJobs statusSyncJobs `json:"syncJobs"`
}

// zoektMode is the typed enum the response carries. String-typed
// so encoding/json needs no MarshalJSON override.
type zoektMode string

const (
	zoektModeSingle       zoektMode = "single"
	zoektModeFanout       zoektMode = "fanout"
	zoektModeRouted       zoektMode = "routed"
	zoektModeOrgDirectory zoektMode = "org-directory"
)

// resolveZoektMode mirrors the dashboard's ternary:
//
//	orgIndex ? "org-directory"
//	  : shardGroups.length > 0 ? "routed"
//	  : zoektWebserverUrls.length > 1 ? "fanout"
//	  : "single"
//
// With orgIndex absent and no shard groups (the current codeintel
// deployment shape) the resolution collapses to "fanout" or
// "single" based on the configured webserver-url count.
func resolveZoektMode(zoektWebserverUrls []string) zoektMode {
	if len(zoektWebserverUrls) > 1 {
		return zoektModeFanout
	}
	return zoektModeSingle
}

type statusZoektReplica struct {
	EndpointURL string `json:"endpointUrl"`
	Status      string `json:"status"`
	IsWriter    bool   `json:"isWriter"`
	Priority    int    `json:"priority"`
}

type statusZoektOrgIndex struct {
	Status        string               `json:"status"`
	IndexPath     string               `json:"indexPath"`
	LastIndexedAt *iso8601MilliTime    `json:"lastIndexedAt"`
	Replicas      []statusZoektReplica `json:"replicas"`
}

type statusZoektShardGroup struct{}
type statusZoektEndpoint struct {
	URL    string `json:"url"`
	Status string `json:"status"`
}

// statusZoekt is the top-level zoekt block. `OrgIndex` is a
// pointer so the JSON wire form renders the absent case as `null`
// rather than `{}`. `ShardGroups` and `Endpoints` are non-nil
// empty slices so they render as `[]`.
type statusZoekt struct {
	Mode        zoektMode               `json:"mode"`
	OrgIndex    *statusZoektOrgIndex    `json:"orgIndex"`
	ShardGroups []statusZoektShardGroup `json:"shardGroups"`
	Endpoints   []statusZoektEndpoint   `json:"endpoints"`
}

type statusResponse struct {
	Org         statusOrg         `json:"org"`
	Repos       statusRepos       `json:"repos"`
	Connections statusConnections `json:"connections"`
	Zoekt       statusZoekt       `json:"zoekt"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.statusLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	var (
		rollup           db.OrgStatusRollup
		rawSyncFailures  []db.RecentFailedConnectionSyncJobRow
		rawIndexFailures []db.RecentFailedRepoIndexingJobRow
	)
	g, gctx := errgroup.WithContext(r.Context())
	g.Go(func() error {
		v, err := s.cfg.Queries.GetOrgStatusRollup(gctx, authCtx.Org.ID)
		if err != nil {
			return err
		}
		rollup = v
		return nil
	})
	g.Go(func() error {
		v, err := s.cfg.Queries.ListRecentFailedConnectionSyncJobs(gctx, authCtx.Org.ID, recentFailureLimit)
		if err != nil {
			return err
		}
		rawSyncFailures = v
		return nil
	})
	g.Go(func() error {
		v, err := s.cfg.Queries.ListRecentFailedRepoIndexingJobs(gctx, authCtx.Org.ID, recentFailureLimit)
		if err != nil {
			return err
		}
		rawIndexFailures = v
		return nil
	})
	// On concurrent failure errgroup returns whichever error landed
	// first; the wire response is the static 500 envelope either way
	// so callers see deterministic bytes even though logs do not.
	if err := g.Wait(); err != nil {
		s.statusLogger.Error("status rollup query failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	syncFailures := make([]recentFailureItem, 0, len(rawSyncFailures))
	for _, f := range rawSyncFailures {
		syncFailures = append(syncFailures, recentFailureItem{
			ID: f.ID,
			Connection: recentFailureConnection{
				ID:             f.ConnectionID,
				Name:           f.ConnectionName,
				ConnectionType: f.ConnectionType,
			},
			ErrorMessage: f.ErrorMessage,
			CreatedAt:    iso8601MilliTime(f.CreatedAt),
			UpdatedAt:    iso8601MilliTime(f.UpdatedAt),
		})
	}
	indexFailures := make([]repoIndexFailureItem, 0, len(rawIndexFailures))
	for _, f := range rawIndexFailures {
		indexFailures = append(indexFailures, repoIndexFailureItem{
			ID: f.ID,
			Repo: repoIndexFailureRepo{
				ID:   f.RepoID,
				Name: f.RepoName,
			},
			ErrorMessage: f.ErrorMessage,
			CreatedAt:    iso8601MilliTime(f.CreatedAt),
			UpdatedAt:    iso8601MilliTime(f.UpdatedAt),
		})
	}

	resp := statusResponse{
		Org: statusOrg{
			ID:     authCtx.Org.ID,
			Name:   authCtx.Org.Name,
			Domain: authCtx.Org.Domain,
		},
		Repos: statusRepos{
			Total:   rollup.RepoCount,
			Indexed: rollup.IndexedRepoCount,
			IndexingJobs: statusRepoIndexingJobs{
				Pending:        rollup.PendingRepoIndexJobs,
				InProgress:     rollup.InProgressRepoIndexJobs,
				Failed:         rollup.FailedRepoIndexJobs,
				RecentFailures: indexFailures,
			},
		},
		Connections: statusConnections{
			Total:  rollup.ConnectionCount,
			Synced: rollup.SyncedConnectionCount,
			SyncJobs: statusSyncJobs{
				Pending:        rollup.PendingSyncJobs,
				InProgress:     rollup.InProgressSyncJobs,
				Failed:         rollup.FailedSyncJobs,
				RecentFailures: syncFailures,
			},
		},
		Zoekt: s.buildZoektStatus(authCtx.Org.ID, rollup),
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.statusLogger.Error("encode status response", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (s *Server) buildZoektStatus(orgID int32, rollup db.OrgStatusRollup) statusZoekt {
	if s.cfg.ZoektStorageLayout != repopaths.StorageLayoutOrgDirectory {
		return statusZoekt{
			Mode:        resolveZoektMode(s.cfg.ZoektWebserverUrls),
			OrgIndex:    nil,
			ShardGroups: []statusZoektShardGroup{},
			Endpoints:   []statusZoektEndpoint{},
		}
	}
	replicas := make([]statusZoektReplica, 0, len(s.cfg.ZoektWebserverUrls))
	for i, endpoint := range s.cfg.ZoektWebserverUrls {
		replicas = append(replicas, statusZoektReplica{
			EndpointURL: endpoint,
			Status:      "READY",
			IsWriter:    i == 0,
			Priority:    i,
		})
	}
	status := "PENDING"
	var lastIndexedAt *iso8601MilliTime
	if rollup.IndexedRepoCount > 0 {
		status = "READY"
		t := iso8601MilliTime(time.Now().UTC())
		lastIndexedAt = &t
	}
	return statusZoekt{
		Mode: zoektModeOrgDirectory,
		OrgIndex: &statusZoektOrgIndex{
			Status:        status,
			IndexPath:     zoektOrgIndexPath(s.cfg, orgID),
			LastIndexedAt: lastIndexedAt,
			Replicas:      replicas,
		},
		ShardGroups: []statusZoektShardGroup{},
		Endpoints:   statusZoektEndpoints(s.cfg.ZoektWebserverUrls),
	}
}

func statusZoektEndpoints(urls []string) []statusZoektEndpoint {
	out := make([]statusZoektEndpoint, 0, len(urls))
	for _, u := range urls {
		out = append(out, statusZoektEndpoint{URL: u, Status: "READY"})
	}
	return out
}

func zoektOrgIndexPath(cfg Config, orgID int32) string {
	root := cfg.ZoektEFSRoot
	if root == "" {
		root = filepath.Join(cfg.ZoektDataCacheDir, "zoekt-orgs")
	}
	return filepath.Join(root, strconv.Itoa(int(orgID)), "index")
}
