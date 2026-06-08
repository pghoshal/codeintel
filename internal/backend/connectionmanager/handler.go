// Asynq handler for connection-sync-queue. Reads the
// ConnectionSyncTaskPayload, loads the Connection row + parses
// its config JSONB, calls CompileFromConfig to fetch + compile
// the repo records, upserts the Repo rows + RepoToConnection
// rows, marks the ConnectionSyncJob COMPLETED.
//
// Direct port of ConnectionManager.runJob (connectionManager.ts:
// 130-220) for the github branch. Other connection types
// (gitlab, gitea, gerrit, bitbucket, azuredevops, git) land in
// later slices.
package connectionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"codeintel/pkg/connectionsync"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgxQuerier is the narrow interface over *pgxpool.Pool the
// handler uses. Defined so unit tests can drop in a pgxmock
// without booting Postgres. Method shapes mirror pgx's own so
// *pgxpool.Pool satisfies it directly.
type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Handler is the asynq worker handler for the
// connection-sync-queue. Holds the typed pool + a logger; the
// asynq.HandlerFunc adapter is on Run.
type Handler struct {
	db     pgxQuerier
	logger *slog.Logger
}

// NewHandler constructs a Handler. The supplied pool MUST be
// scoped to the codeintel Postgres instance.
func NewHandler(db pgxQuerier, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{db: db, logger: logger.With("component", "connection-sync")}
}

// Handle satisfies asynq's handler signature. Decodes the
// payload, dispatches to the github branch, and on success
// updates the ConnectionSyncJob to COMPLETED.
//
// Legacy parity notes:
//   - Status guard at the start (legacy line 145): refuse to
//     run if the job is already in a terminal state. We treat
//     any non-PENDING/non-IN_PROGRESS state as a no-op success.
//   - Sets IN_PROGRESS before doing work.
//   - On error, sets FAILED with errorMessage; the asynq retry
//     policy (in the Server config) decides whether to re-queue.
func (h *Handler) Handle(ctx context.Context, t *asynq.Task) error {
	if h == nil || h.db == nil {
		return errors.New("connectionmanager: handler not configured")
	}

	payload, err := connectionsync.Unmarshal(t.Payload())
	if err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	h.logger.Info("connection-sync received",
		"jobId", payload.JobID,
		"connectionId", payload.ConnectionID,
		"orgId", payload.OrgID,
	)

	// 1. Status guard + transition to IN_PROGRESS.
	if err := h.markInProgress(ctx, payload.JobID, payload.ConnectionID, payload.OrgID); err != nil {
		return fmt.Errorf("markInProgress: %w", err)
	}

	// 2. Load Connection row + parse config JSONB.
	conn, err := h.loadConnection(ctx, payload.ConnectionID, payload.OrgID)
	if err != nil {
		_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, err.Error())
		return fmt.Errorf("loadConnection: %w", err)
	}

	// 3. Dispatch on connectionType. Each branch parses the
	// type-specific config + invokes the matching pipeline.
	// One Org can carry multiple Connections of different
	// types; each lands here as a separate task and is
	// dispatched independently.
	var (
		records  []RepoData
		warnings []string
	)
	switch conn.ConnectionType {
	case "github":
		var cfg GitHubConnectionConfig
		if err := json.Unmarshal(conn.Config, &cfg); err != nil {
			_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, fmt.Sprintf("github config parse: %v", err))
			return fmt.Errorf("github config parse: %w", err)
		}
		records, warnings, err = CompileFromConfig(ctx, cfg, payload.ConnectionID)
	case "gitlab":
		var cfg GitLabConnectionConfig
		if err := json.Unmarshal(conn.Config, &cfg); err != nil {
			_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, fmt.Sprintf("gitlab config parse: %v", err))
			return fmt.Errorf("gitlab config parse: %w", err)
		}
		records, warnings, err = CompileGitLabFromConfig(ctx, cfg, payload.ConnectionID)
	case "gitea":
		var cfg GiteaConnectionConfig
		if err := json.Unmarshal(conn.Config, &cfg); err != nil {
			_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, fmt.Sprintf("gitea config parse: %v", err))
			return fmt.Errorf("gitea config parse: %w", err)
		}
		records, warnings, err = CompileGiteaFromConfig(ctx, cfg, payload.ConnectionID)
	case "bitbucket":
		var cfg BitbucketConnectionConfig
		if err := json.Unmarshal(conn.Config, &cfg); err != nil {
			_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, fmt.Sprintf("bitbucket config parse: %v", err))
			return fmt.Errorf("bitbucket config parse: %w", err)
		}
		records, warnings, err = CompileBitbucketFromConfig(ctx, cfg, payload.ConnectionID)
	case "gerrit":
		var cfg GerritConnectionConfig
		if err := json.Unmarshal(conn.Config, &cfg); err != nil {
			_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, fmt.Sprintf("gerrit config parse: %v", err))
			return fmt.Errorf("gerrit config parse: %w", err)
		}
		records, warnings, err = CompileGerritFromConfig(ctx, cfg, payload.ConnectionID)
	case "azuredevops":
		var cfg AzureDevOpsConnectionConfig
		if err := json.Unmarshal(conn.Config, &cfg); err != nil {
			_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, fmt.Sprintf("azuredevops config parse: %v", err))
			return fmt.Errorf("azuredevops config parse: %w", err)
		}
		records, warnings, err = CompileAzureDevOpsFromConfig(ctx, cfg, payload.ConnectionID)
	case "git":
		var cfg GenericGitHostConnectionConfig
		if err := json.Unmarshal(conn.Config, &cfg); err != nil {
			_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, fmt.Sprintf("git config parse: %v", err))
			return fmt.Errorf("git config parse: %w", err)
		}
		records, warnings, err = CompileGenericGitHostFromConfig(ctx, cfg, payload.ConnectionID)
	default:
		errMsg := fmt.Sprintf("connectionType %q not yet supported", conn.ConnectionType)
		_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, errMsg)
		return errors.New(errMsg)
	}
	if err != nil {
		_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, err.Error())
		return fmt.Errorf("compile: %w", err)
	}
	h.logger.Info("connection-sync compiled",
		"connectionId", payload.ConnectionID,
		"repos", len(records),
		"warnings", len(warnings),
	)

	// 5. Upsert each Repo + RepoToConnection.
	for i := range records {
		if err := h.upsertRepo(ctx, payload.OrgID, &records[i]); err != nil {
			_ = h.markFailed(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, fmt.Sprintf("upsertRepo %s: %v", records[i].ExternalID, err))
			return fmt.Errorf("upsertRepo: %w", err)
		}
	}

	// 6. Mark COMPLETED with warningMessages.
	if err := h.markCompleted(ctx, payload.JobID, payload.ConnectionID, payload.OrgID, warnings); err != nil {
		return fmt.Errorf("markCompleted: %w", err)
	}
	h.logger.Info("connection-sync completed",
		"jobId", payload.JobID,
		"repoCount", len(records),
	)
	return nil
}

// connectionRow captures the columns the handler reads from the
// Connection table.
type connectionRow struct {
	ID             int32
	OrgID          int32
	Name           string
	ConnectionType string
	Config         []byte
}

func (h *Handler) loadConnection(ctx context.Context, connectionID, orgID int32) (connectionRow, error) {
	var c connectionRow
	row := h.db.QueryRow(ctx, `
		SELECT id, "orgId", name, "connectionType"::text, config::text
		FROM "Connection"
		WHERE id = $1 AND "orgId" = $2
	`, connectionID, orgID)
	var cfgText string
	if err := row.Scan(&c.ID, &c.OrgID, &c.Name, &c.ConnectionType, &cfgText); err != nil {
		return connectionRow{}, fmt.Errorf("scan: %w", err)
	}
	c.Config = []byte(cfgText)
	return c, nil
}

func (h *Handler) markInProgress(ctx context.Context, jobID string, connectionID, orgID int32) error {
	tag, err := h.db.Exec(ctx, `
		UPDATE "ConnectionSyncJob" j
		SET status = 'IN_PROGRESS', "updatedAt" = NOW()
		WHERE j.id = $1
		  AND j."connectionId" = $2
		  AND j.status IN ('PENDING', 'IN_PROGRESS')
		  AND EXISTS (
		      SELECT 1
		      FROM "Connection" c
		      WHERE c.id = j."connectionId" AND c."orgId" = $3
		  )
	`, jobID, connectionID, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("job %q already in terminal state or missing", jobID)
	}
	return nil
}

func (h *Handler) markFailed(ctx context.Context, jobID string, connectionID, orgID int32, errMsg string) error {
	_, err := h.db.Exec(ctx, `
		UPDATE "ConnectionSyncJob" j
		SET status = 'FAILED', "errorMessage" = $2, "completedAt" = NOW(), "updatedAt" = NOW()
		WHERE j.id = $1
		  AND j."connectionId" = $3
		  AND EXISTS (
		      SELECT 1
		      FROM "Connection" c
		      WHERE c.id = j."connectionId" AND c."orgId" = $4
		  )
	`, jobID, errMsg, connectionID, orgID)
	return err
}

func (h *Handler) markCompleted(ctx context.Context, jobID string, connectionID, orgID int32, warnings []string) error {
	// `warningMessages` is NOT NULL TEXT[]; coerce nil to empty
	// slice so pgx encodes it as `{}` rather than SQL NULL.
	if warnings == nil {
		warnings = []string{}
	}
	tag, err := h.db.Exec(ctx, `
		UPDATE "ConnectionSyncJob" j
		SET status = 'COMPLETED',
		    "warningMessages" = $2,
		    "completedAt" = NOW(),
		    "updatedAt" = NOW()
		WHERE j.id = $1
		  AND j."connectionId" = $3
		  AND EXISTS (
		      SELECT 1
		      FROM "Connection" c
		      WHERE c.id = j."connectionId" AND c."orgId" = $4
		  )
	`, jobID, warnings, connectionID, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("job %q missing or mismatched connection", jobID)
	}
	// Also stamp Connection.syncedAt.
	tag, err = h.db.Exec(ctx, `
		UPDATE "Connection" SET "syncedAt" = NOW(), "updatedAt" = NOW() WHERE id = $1 AND "orgId" = $2
	`, connectionID, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("connection %d missing in org %d", connectionID, orgID)
	}
	return nil
}

// upsertRepo inserts (or updates on conflict) a Repo row and
// attaches it to the connection via RepoToConnection. The
// composite unique on (external_id, external_codeHostUrl, orgId)
// is the conflict target; on hit the existing row is updated
// in-place with the new metadata + URLs.
//
// Returns the inserted/updated Repo.id.
func (h *Handler) upsertRepo(ctx context.Context, orgID int32, rec *RepoData) error {
	// Tenant-scoping invariant: every row this worker writes
	// MUST carry the Connection's orgId. The compile path
	// defaults to SingleTenantOrgID when given no explicit
	// orgId; the handler authoritatively overrides here so
	// cross-tenant writes are impossible regardless of the
	// compile-input shape.
	rec.OrgID = orgID

	metadataJSON, err := json.Marshal(rec.Metadata)
	if err != nil {
		return fmt.Errorf("metadata marshal: %w", err)
	}

	var repoID int32
	if err := h.db.QueryRow(ctx, `
		INSERT INTO "Repo" (
		  name, "displayName", "isFork", "isArchived", "isPublic",
		  "isAutoCleanupDisabled", metadata, "cloneUrl", "webUrl", "imageUrl",
		  "defaultBranch", external_id, "external_codeHostType",
		  "external_codeHostUrl", "orgId", "createdAt", "updatedAt"
		) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11,$12,$13::"CodeHostType",$14,$15,NOW(),NOW())
		ON CONFLICT (external_id, "external_codeHostUrl", "orgId")
		DO UPDATE SET
		  name = EXCLUDED.name,
		  "displayName" = EXCLUDED."displayName",
		  "isFork" = EXCLUDED."isFork",
		  "isArchived" = EXCLUDED."isArchived",
		  "isPublic" = EXCLUDED."isPublic",
		  metadata = EXCLUDED.metadata,
		  "cloneUrl" = EXCLUDED."cloneUrl",
		  "webUrl" = EXCLUDED."webUrl",
		  "imageUrl" = EXCLUDED."imageUrl",
		  "defaultBranch" = EXCLUDED."defaultBranch",
		  "external_codeHostType" = EXCLUDED."external_codeHostType",
		  "updatedAt" = NOW()
		RETURNING id
	`,
		rec.Name, rec.DisplayName, rec.IsFork, rec.IsArchived, rec.IsPublic,
		ptrBoolOrFalse(rec.IsAutoCleanupDisabled),
		metadataJSON, rec.CloneURL, rec.WebURL, rec.ImageURL,
		rec.DefaultBranch, rec.ExternalID, rec.ExternalCodeHostType,
		rec.ExternalCodeHostURL, rec.OrgID,
	).Scan(&repoID); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}

	// Attach to each connection. Idempotent via ON CONFLICT.
	for _, cid := range rec.ConnectionIDs {
		if _, err := h.db.Exec(ctx, `
			INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt")
			VALUES ($1, $2, NOW())
			ON CONFLICT ("connectionId", "repoId") DO NOTHING
		`, cid, repoID); err != nil {
			return fmt.Errorf("RepoToConnection insert: %w", err)
		}
	}
	return nil
}

func ptrBoolOrFalse(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// AsynqHandlerFunc adapts Handler.Handle to the signature
// asynq.ServeMux.HandleFunc expects.
func (h *Handler) AsynqHandlerFunc() func(context.Context, *asynq.Task) error {
	return h.Handle
}
