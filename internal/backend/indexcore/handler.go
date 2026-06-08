// Package indexcore handles backend-owned core indexing subjobs that do
// not require a Rust artifact executor.
package indexcore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"codeintel/internal/backend/graphstore"
	"codeintel/internal/backend/indexsubjobs"
	"codeintel/internal/backend/indexsubjobtask"
	"codeintel/pkg/graphschema"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	defaultLeaseDuration     = 2 * time.Minute
	defaultHeartbeatInterval = 30 * time.Second
	defaultSchemaVersion     = int32(1)
	defaultBuilder           = "codeintel-code-graph-v7"
	defaultSCIPGraphBatch    = 500
	defaultSCIPOccurrenceCap = 30000
	defaultSCIPSymbolRoleCap = 48
	defaultSCIPFileCap       = 600
	scipPostgresBatchSize    = 500
)

var scipQuotedSymbolRE = regexp.MustCompile("`([^`]+)`")

type database interface {
	Begin(context.Context) (pgx.Tx, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type Store interface {
	ClaimScoped(context.Context, indexsubjobs.ClaimScope, string, string, time.Time) (bool, error)
	Heartbeat(context.Context, string, string, string, time.Time) (bool, error)
	MarkSucceeded(context.Context, string, string, string) (bool, error)
	MarkFailed(context.Context, string, string, string, string, string) (bool, error)
}

type graphWriter interface {
	WriteRenderedStatements(context.Context, graphstore.RenderedStatementWrite) (graphschema.CodeGraphWriteResult, error)
}

type Config struct {
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	LeaseOwner        string
	Now               func() time.Time
	Graph             graphWriter
}

type Handler struct {
	db                database
	store             Store
	logger            *slog.Logger
	leaseDuration     time.Duration
	heartbeatInterval time.Duration
	leaseOwner        string
	now               func() time.Time
	newID             func() string
	graph             graphWriter
}

func NewHandler(db database, store Store, logger *slog.Logger, cfg Config) (*Handler, error) {
	if db == nil {
		return nil, errors.New("indexcore: database is required")
	}
	if store == nil {
		return nil, errors.New("indexcore: store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	leaseDuration := cfg.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseDuration
	}
	heartbeatInterval := cfg.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultHeartbeatInterval
	}
	if heartbeatInterval >= leaseDuration {
		return nil, errors.New("indexcore: heartbeat interval must be shorter than lease duration")
	}
	leaseOwner := cfg.LeaseOwner
	if leaseOwner == "" {
		hostname, _ := os.Hostname()
		leaseOwner = "index-core-" + hostname + "-" + nonceHex(12)
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Handler{
		db:                db,
		store:             store,
		logger:            logger.With("component", "index-core"),
		leaseDuration:     leaseDuration,
		heartbeatInterval: heartbeatInterval,
		leaseOwner:        leaseOwner,
		now:               now,
		newID:             func() string { return "ic_" + nonceHex(16) },
		graph:             cfg.Graph,
	}, nil
}

func (h *Handler) AsynqHandlerFunc() func(context.Context, *asynq.Task) error {
	return h.Handle
}

func (h *Handler) Handle(ctx context.Context, task *asynq.Task) error {
	if h == nil || h.db == nil || h.store == nil {
		return errors.New("indexcore: handler is not configured")
	}
	payload, err := indexsubjobtask.Unmarshal(task.Payload())
	if err != nil {
		h.logger.Warn("dropping malformed core index subjob task", "taskType", task.Type(), "err", err)
		return nil
	}
	if task.Type() != payload.QueueName {
		h.logger.Warn("dropping wrong-queue core index subjob task",
			"taskType", task.Type(),
			"payloadQueue", payload.QueueName,
			"subjobId", payload.SubjobID,
		)
		return nil
	}
	if payload.Layer != indexsubjobtask.LayerGraphMerge && payload.Layer != indexsubjobtask.LayerActivate {
		return fmt.Errorf("indexcore: unsupported core layer %s", payload.Layer)
	}

	attemptID := generatedAttemptID(payload.SubjobID, payload.Attempt)
	claimed, err := h.store.ClaimScoped(ctx, toClaimScope(payload), h.leaseOwner, attemptID, h.now().Add(h.leaseDuration))
	if err != nil {
		return fmt.Errorf("claim core subjob %s: %w", payload.SubjobID, err)
	}
	if !claimed {
		h.logger.Info("core subjob claim skipped",
			"subjobId", payload.SubjobID,
			"repoIndexingJobId", payload.RepoIndexingJobID,
			"orgId", payload.OrgID,
			"repoId", payload.RepoID,
			"layer", payload.Layer,
			"attempt", payload.Attempt,
		)
		return nil
	}

	if err := h.withHeartbeat(ctx, payload, attemptID, func(runCtx context.Context) error {
		return h.execute(runCtx, payload)
	}); err != nil {
		ok, markErr := h.store.MarkFailed(ctx, payload.SubjobID, h.leaseOwner, attemptID, "CORE_SUBJOB_FAILED", err.Error())
		if markErr != nil {
			return fmt.Errorf("mark core subjob %s failed: %w", payload.SubjobID, markErr)
		}
		if !ok {
			return fmt.Errorf("mark core subjob %s failed: lease lost", payload.SubjobID)
		}
		h.logger.Warn("core subjob failed",
			"subjobId", payload.SubjobID,
			"repoIndexingJobId", payload.RepoIndexingJobID,
			"orgId", payload.OrgID,
			"repoId", payload.RepoID,
			"layer", payload.Layer,
			"err", err,
		)
		return nil
	}

	ok, err := h.store.MarkSucceeded(ctx, payload.SubjobID, h.leaseOwner, attemptID)
	if err != nil {
		return fmt.Errorf("mark core subjob %s succeeded: %w", payload.SubjobID, err)
	}
	if !ok {
		return fmt.Errorf("mark core subjob %s succeeded: lease lost", payload.SubjobID)
	}
	if payload.Layer == indexsubjobtask.LayerActivate {
		if err := h.completeJobIfReady(ctx, payload); err != nil {
			return err
		}
	}
	h.logger.Info("core subjob completed",
		"subjobId", payload.SubjobID,
		"repoIndexingJobId", payload.RepoIndexingJobID,
		"orgId", payload.OrgID,
		"repoId", payload.RepoID,
		"layer", payload.Layer,
	)
	return nil
}

func (h *Handler) withHeartbeat(ctx context.Context, payload indexsubjobtask.Payload, attemptID string, fn func(context.Context) error) error {
	firstLease := h.now().Add(h.leaseDuration)
	ok, err := h.store.Heartbeat(ctx, payload.SubjobID, h.leaseOwner, attemptID, firstLease)
	if err != nil {
		return fmt.Errorf("initial heartbeat: %w", err)
	}
	if !ok {
		return errors.New("initial heartbeat: lease lost")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	heartbeatErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(h.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				leaseUntil := h.now().Add(h.leaseDuration)
				ok, err := h.store.Heartbeat(runCtx, payload.SubjobID, h.leaseOwner, attemptID, leaseUntil)
				if err != nil {
					select {
					case heartbeatErr <- fmt.Errorf("heartbeat: %w", err):
					default:
					}
					cancel()
					return
				}
				if !ok {
					select {
					case heartbeatErr <- errors.New("heartbeat: lease lost"):
					default:
					}
					cancel()
					return
				}
			case <-runCtx.Done():
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- fn(runCtx)
	}()

	select {
	case hbErr := <-heartbeatErr:
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			h.logger.Warn("core subjob function did not stop after heartbeat loss",
				"subjobId", payload.SubjobID,
				"repoIndexingJobId", payload.RepoIndexingJobID,
				"orgId", payload.OrgID,
				"repoId", payload.RepoID,
				"layer", payload.Layer,
			)
		}
		<-heartbeatDone
		return hbErr
	case err := <-done:
		cancel()
		<-heartbeatDone
		return err
	}
}

func (h *Handler) execute(ctx context.Context, payload indexsubjobtask.Payload) error {
	switch payload.Layer {
	case indexsubjobtask.LayerGraphMerge:
		return h.mergeGraph(ctx, payload)
	case indexsubjobtask.LayerActivate:
		return h.activate(ctx, payload)
	default:
		return fmt.Errorf("unsupported layer %s", payload.Layer)
	}
}

func (h *Handler) mergeGraph(ctx context.Context, payload indexsubjobtask.Payload) error {
	graphIndexID, err := h.resolvePartialGraphIndex(ctx, payload)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		symbols, relationships, occurrences, loadErr := h.loadSCIPProjectionRows(ctx, payload)
		if loadErr != nil {
			return loadErr
		}
		if len(symbols) == 0 && len(relationships) == 0 && len(occurrences) == 0 {
			h.logger.Info("graph merge has no AST snapshot and no SCIP evidence; continuing without graph snapshot",
				"repoIndexingJobId", payload.RepoIndexingJobID,
				"orgId", payload.OrgID,
				"repoId", payload.RepoID,
				"revision", payload.Revision,
				"commitHash", shortCommit(payload.CommitHash),
			)
			return nil
		}
		graphIndexID, err = h.createPartialGraphIndexForSCIPOnly(ctx, payload)
		if err != nil {
			return err
		}
		if err := h.projectSCIPSemanticGraphRows(ctx, graphIndexID, payload, symbols, relationships, occurrences); err != nil {
			return err
		}
	} else if err := h.projectSCIPSemanticGraph(ctx, graphIndexID, payload); err != nil {
		return err
	}
	tag, err := h.db.Exec(ctx, `
		UPDATE "CodeGraphIndex" g
		SET status = 'READY'::"CodeGraphIndexStatus",
		    "indexedAt" = CURRENT_TIMESTAMP,
		    "errorMessage" = NULL,
		    "updatedAt" = CURRENT_TIMESTAMP
		WHERE g.id = (
		    SELECT id
		    FROM "CodeGraphIndex"
		    WHERE "orgId" = $1
		      AND "repoId" = $2
		      AND "workspaceId" = $3
		      AND "sourceRevision" = $4
		      AND "commitHash" = $5
		      AND provider = 'NEBULA'
		      AND "schemaVersion" = $6
		      AND "builderVersion" = $7
		      AND status = 'PARTIAL'::"CodeGraphIndexStatus"
		    ORDER BY "updatedAt" DESC, id DESC
		    LIMIT 1
		)
		  AND NOT EXISTS (
		    SELECT 1
		    FROM "CodeIntelIndexSubjob" dep
		    WHERE dep."repoIndexingJobId" = $8
		      AND dep."workspaceId" = $3
		      AND dep."repoId" = $2
		      AND dep.branch = $9
		      AND dep.revision = $4
		      AND dep."commitHash" = $5
		      AND dep.layer IN ('AST_TREE_SITTER', 'SCIP')
		      AND dep.status NOT IN ('SUCCEEDED', 'SKIPPED')
		      AND NOT (
		        dep.layer = 'SCIP'
		        AND dep.status IN ('FAILED', 'CANCELED')
		      )
		  )
	`, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Revision, payload.CommitHash,
		defaultSchemaVersion, defaultBuilder, payload.RepoIndexingJobID, payload.Branch)
	if err != nil {
		return fmt.Errorf("promote partial graph: %w", err)
	}
	if tag.RowsAffected() != 1 {
		ready, err := h.graphSnapshotAlreadyReady(ctx, payload)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		return errors.New("indexcore: no completed PARTIAL graph snapshot available to merge")
	}
	return nil
}

func (h *Handler) createPartialGraphIndexForSCIPOnly(ctx context.Context, payload indexsubjobtask.Payload) (string, error) {
	if payload.WorkspaceID == nil {
		return "", errors.New("indexcore: workspaceId is required for SCIP-only graph merge")
	}
	id := h.newID()
	var graphIndexID string
	if err := h.db.QueryRow(ctx, `
		INSERT INTO "CodeGraphIndex" (
			id, provider, status, "sourceRevision", "commitHash", "graphSpace",
			"workspaceId", "schemaVersion", "builderVersion", "indexRunId",
			"vertexCount", "edgeCount", "anchorCount", "linkedEdgeCount",
			"errorMessage", "indexedAt", "orgId", "repoId", "updatedAt"
		) VALUES (
			$1, 'NEBULA', 'PARTIAL', $2, $3, $4,
			$5, $6, $7, $8,
			0, 0, 0, 0,
			NULL, NULL, $9, $10, CURRENT_TIMESTAMP
		)
		ON CONFLICT ("repoId", "workspaceId", "commitHash", "provider", "schemaVersion", "builderVersion")
		DO UPDATE SET
			status = CASE
				WHEN "CodeGraphIndex".status = 'READY' THEN "CodeGraphIndex".status
				ELSE 'PARTIAL'::"CodeGraphIndexStatus"
			END,
			"sourceRevision" = EXCLUDED."sourceRevision",
			"graphSpace" = EXCLUDED."graphSpace",
			"indexRunId" = EXCLUDED."indexRunId",
			"errorMessage" = NULL,
			"updatedAt" = CURRENT_TIMESTAMP
		RETURNING id
	`, id, payload.Revision, payload.CommitHash, graphschema.SpaceName,
		*payload.WorkspaceID, defaultSchemaVersion, defaultBuilder,
		payload.RepoIndexingJobID, payload.OrgID, payload.RepoID).Scan(&graphIndexID); err != nil {
		return "", fmt.Errorf("create SCIP-only graph snapshot: %w", err)
	}
	return graphIndexID, nil
}

func (h *Handler) graphSnapshotAlreadyReady(ctx context.Context, payload indexsubjobtask.Payload) (bool, error) {
	var exists bool
	if err := h.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM "CodeGraphIndex" g
			WHERE g."orgId" = $1
			  AND g."repoId" = $2
			  AND g."workspaceId" = $3
			  AND g."sourceRevision" = $4
			  AND g."commitHash" = $5
			  AND g.provider = 'NEBULA'
			  AND g."schemaVersion" = $6
			  AND g."builderVersion" = $7
			  AND g.status = 'READY'::"CodeGraphIndexStatus"
			  AND NOT EXISTS (
			    SELECT 1
			    FROM "CodeIntelIndexSubjob" dep
			    WHERE dep."repoIndexingJobId" = $8
			      AND dep."workspaceId" = $3
			      AND dep."repoId" = $2
			      AND dep.branch = $9
			      AND dep.revision = $4
			      AND dep."commitHash" = $5
			      AND dep.layer IN ('AST_TREE_SITTER', 'SCIP')
			      AND dep.status NOT IN ('SUCCEEDED', 'SKIPPED')
			      AND NOT (
			        dep.layer = 'SCIP'
			        AND dep.status IN ('FAILED', 'CANCELED')
			      )
			  )
		)
	`, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Revision, payload.CommitHash,
		defaultSchemaVersion, defaultBuilder, payload.RepoIndexingJobID, payload.Branch).Scan(&exists); err != nil {
		return false, fmt.Errorf("check ready graph snapshot: %w", err)
	}
	return exists, nil
}

func (h *Handler) resolvePartialGraphIndex(ctx context.Context, payload indexsubjobtask.Payload) (string, error) {
	var graphIndexID string
	if err := h.db.QueryRow(ctx, `
		SELECT id
		FROM "CodeGraphIndex"
		WHERE "orgId" = $1
		  AND "repoId" = $2
		  AND "workspaceId" = $3
		  AND "sourceRevision" = $4
		  AND "commitHash" = $5
		  AND provider = 'NEBULA'
		  AND "schemaVersion" = $6
		  AND "builderVersion" = $7
		  AND status IN ('PARTIAL'::"CodeGraphIndexStatus", 'READY'::"CodeGraphIndexStatus")
		ORDER BY
		  CASE status
		    WHEN 'PARTIAL'::"CodeGraphIndexStatus" THEN 0
		    WHEN 'READY'::"CodeGraphIndexStatus" THEN 1
		    ELSE 2
		  END,
		  "updatedAt" DESC,
		  id DESC
		LIMIT 1
	`, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Revision, payload.CommitHash,
		defaultSchemaVersion, defaultBuilder).Scan(&graphIndexID); err != nil {
		return "", fmt.Errorf("resolve graph snapshot for merge: %w", err)
	}
	return graphIndexID, nil
}

type scipSymbolProjection struct {
	Symbol      string
	DisplayName string
	Kind        string
	Language    string
	FilePath    string
	StartLine   sql.NullInt32
	EndLine     sql.NullInt32
}

type scipRelationshipProjection struct {
	SourceSymbol     string
	TargetSymbol     string
	IsReference      bool
	IsImplementation bool
	IsTypeDefinition bool
	IsDefinition     bool
	SourceFile       string
	StartLine        sql.NullInt32
	EndLine          sql.NullInt32
}

type scipOccurrenceProjection struct {
	Symbol          string
	FilePath        string
	StartLine       sql.NullInt32
	EndLine         sql.NullInt32
	Role            string
	SyntaxKind      string
	LineContent     string
	EnclosingSymbol string
}

func (h *Handler) projectSCIPSemanticGraph(ctx context.Context, graphIndexID string, payload indexsubjobtask.Payload) error {
	if payload.WorkspaceID == nil {
		return errors.New("indexcore: workspaceId is required for SCIP graph projection")
	}
	start := time.Now()
	symbols, relationships, occurrences, err := h.loadSCIPProjectionRows(ctx, payload)
	if err != nil {
		return err
	}
	h.logger.Info("loaded SCIP semantic graph projection rows",
		"repoIndexingJobId", payload.RepoIndexingJobID,
		"orgId", payload.OrgID,
		"repoId", payload.RepoID,
		"symbols", len(symbols),
		"relationships", len(relationships),
		"occurrences", len(occurrences),
		"durationMs", time.Since(start).Milliseconds(),
	)
	return h.projectSCIPSemanticGraphRows(ctx, graphIndexID, payload, symbols, relationships, occurrences)
}

func (h *Handler) projectSCIPSemanticGraphRows(ctx context.Context, graphIndexID string, payload indexsubjobtask.Payload, symbols []scipSymbolProjection, relationships []scipRelationshipProjection, occurrences []scipOccurrenceProjection) error {
	if payload.WorkspaceID == nil {
		return errors.New("indexcore: workspaceId is required for SCIP graph projection")
	}
	if len(symbols) == 0 && len(relationships) == 0 && len(occurrences) == 0 {
		return nil
	}
	snapshot := h.buildSCIPSnapshot(payload, symbols, relationships, occurrences)
	if len(snapshot.Vertices) == 0 && len(snapshot.Edges) == 0 {
		return nil
	}
	if h.graph != nil {
		h.logger.Info("writing SCIP semantic graph to Nebula",
			"repoIndexingJobId", payload.RepoIndexingJobID,
			"orgId", payload.OrgID,
			"repoId", payload.RepoID,
			"revision", payload.Revision,
			"vertices", len(snapshot.Vertices),
			"edges", len(snapshot.Edges),
			"occurrenceEdges", len(occurrences),
			"batchSize", scipGraphBatchSize(),
		)
		if err := h.writeSCIPSnapshotToGraph(ctx, payload, snapshot); err != nil {
			return err
		}
	}
	if len(snapshot.Vertices) > 0 {
		h.logger.Info("upserting SCIP semantic facts",
			"repoIndexingJobId", payload.RepoIndexingJobID,
			"orgId", payload.OrgID,
			"repoId", payload.RepoID,
			"count", len(snapshot.Vertices),
		)
		if err := h.insertSCIPSemanticFacts(ctx, graphIndexID, payload, snapshot.Vertices); err != nil {
			return err
		}
	}
	if len(snapshot.Edges) > 0 {
		h.logger.Info("upserting SCIP semantic edges",
			"repoIndexingJobId", payload.RepoIndexingJobID,
			"orgId", payload.OrgID,
			"repoId", payload.RepoID,
			"count", len(snapshot.Edges),
		)
		if err := h.insertSCIPSemanticEdges(ctx, graphIndexID, payload, snapshot.Edges); err != nil {
			return err
		}
	}
	h.logger.Info("projected SCIP semantic graph",
		"repoIndexingJobId", payload.RepoIndexingJobID,
		"orgId", payload.OrgID,
		"repoId", payload.RepoID,
		"revision", payload.Revision,
		"commitHash", shortCommit(payload.CommitHash),
		"vertices", len(snapshot.Vertices),
		"edges", len(snapshot.Edges),
		"occurrences", len(occurrences),
	)
	return nil
}

func (h *Handler) loadSCIPProjectionRows(ctx context.Context, payload indexsubjobtask.Payload) ([]scipSymbolProjection, []scipRelationshipProjection, []scipOccurrenceProjection, error) {
	start := time.Now()
	symbolRows, err := h.dbQuery(ctx, `
		SELECT s.symbol, COALESCE(s."displayName", ''), COALESCE(s.kind, ''),
		       COALESCE(s.language, ''), COALESCE(s."filePath", ''),
		       s."startLine", s."endLine"
		FROM "CodeIntelSymbol" s
		JOIN "CodeIntelIndex" ci
		  ON ci.id = s."codeIntelIndexId"
		 AND ci."orgId" = s."orgId"
		 AND ci."repoId" = s."repoId"
		JOIN "CodeIntelLanguageIndex" li
		  ON li.id = s."codeIntelLanguageIndexId"
		 AND li."codeIntelIndexId" = ci.id
		WHERE ci."orgId" = $1
		  AND ci."repoId" = $2
		  AND ci."workspaceId" = $3
		  AND ci.branch = $4
		  AND ci.revision = $5
		  AND ci."commitHash" = $6
		  AND ci.kind = 'SCIP'::"CodeIntelIndexKind"
		  AND ci.status IN ('READY'::"CodeIntelIndexStatus", 'PARTIAL'::"CodeIntelIndexStatus")
		  AND li.status = 'READY'::"CodeIntelIndexStatus"
		ORDER BY s."filePath" NULLS LAST, s."startLine" NULLS LAST, s.symbol
	`, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Branch, payload.Revision, payload.CommitHash)
	if err != nil {
		return nil, nil, nil, err
	}
	defer symbolRows.Close()
	var symbols []scipSymbolProjection
	for symbolRows.Next() {
		var row scipSymbolProjection
		if err := symbolRows.Scan(&row.Symbol, &row.DisplayName, &row.Kind, &row.Language, &row.FilePath, &row.StartLine, &row.EndLine); err != nil {
			return nil, nil, nil, fmt.Errorf("scan SCIP symbols for graph projection: %w", err)
		}
		symbols = append(symbols, row)
	}
	if err := symbolRows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("SCIP symbol projection rows: %w", err)
	}
	h.logger.Info("loaded SCIP symbol projection rows",
		"repoIndexingJobId", payload.RepoIndexingJobID,
		"orgId", payload.OrgID,
		"repoId", payload.RepoID,
		"count", len(symbols),
		"durationMs", time.Since(start).Milliseconds(),
	)

	start = time.Now()
	relationshipRows, err := h.dbQuery(ctx, `
		SELECT r."sourceSymbol", r."targetSymbol", r."isReference",
		       r."isImplementation", r."isTypeDefinition", r."isDefinition",
		       COALESCE(src."filePath", ''),
		       src."startLine",
		       src."endLine"
		FROM "CodeIntelRelationship" r
		JOIN "CodeIntelIndex" ci
		  ON ci.id = r."codeIntelIndexId"
		 AND ci."orgId" = r."orgId"
		 AND ci."repoId" = r."repoId"
		JOIN "CodeIntelLanguageIndex" li
		  ON li.id = r."codeIntelLanguageIndexId"
		 AND li."codeIntelIndexId" = ci.id
		LEFT JOIN "CodeIntelSymbol" src
		  ON src."codeIntelIndexId" = ci.id
		 AND src.symbol = r."sourceSymbol"
		WHERE ci."orgId" = $1
		  AND ci."repoId" = $2
		  AND ci."workspaceId" = $3
		  AND ci.branch = $4
		  AND ci.revision = $5
		  AND ci."commitHash" = $6
		  AND ci.kind = 'SCIP'::"CodeIntelIndexKind"
		  AND ci.status IN ('READY'::"CodeIntelIndexStatus", 'PARTIAL'::"CodeIntelIndexStatus")
		  AND li.status = 'READY'::"CodeIntelIndexStatus"
		ORDER BY r."sourceSymbol", r."targetSymbol"
	`, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Branch, payload.Revision, payload.CommitHash)
	if err != nil {
		return nil, nil, nil, err
	}
	defer relationshipRows.Close()
	var relationships []scipRelationshipProjection
	for relationshipRows.Next() {
		var row scipRelationshipProjection
		if err := relationshipRows.Scan(&row.SourceSymbol, &row.TargetSymbol, &row.IsReference, &row.IsImplementation, &row.IsTypeDefinition, &row.IsDefinition, &row.SourceFile, &row.StartLine, &row.EndLine); err != nil {
			return nil, nil, nil, fmt.Errorf("scan SCIP relationships for graph projection: %w", err)
		}
		relationships = append(relationships, row)
	}
	if err := relationshipRows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("SCIP relationship projection rows: %w", err)
	}
	h.logger.Info("loaded SCIP relationship projection rows",
		"repoIndexingJobId", payload.RepoIndexingJobID,
		"orgId", payload.OrgID,
		"repoId", payload.RepoID,
		"count", len(relationships),
		"durationMs", time.Since(start).Milliseconds(),
	)

	start = time.Now()
	occurrenceRows, err := h.dbQuery(ctx, `
		WITH candidate_files AS (
		  SELECT DISTINCT o."filePath"
		  FROM "CodeIntelOccurrence" o
		  JOIN "CodeIntelIndex" ci
		    ON ci.id = o."codeIntelIndexId"
		   AND ci."orgId" = o."orgId"
		   AND ci."repoId" = o."repoId"
		  JOIN "CodeIntelLanguageIndex" li
		    ON li.id = o."codeIntelLanguageIndexId"
		   AND li."codeIntelIndexId" = ci.id
		  WHERE ci."orgId" = $1
		    AND ci."repoId" = $2
		    AND ci."workspaceId" = $3
		    AND ci.branch = $4
		    AND ci.revision = $5
		    AND ci."commitHash" = $6
		    AND ci.kind = 'SCIP'::"CodeIntelIndexKind"
		    AND ci.status IN ('READY'::"CodeIntelIndexStatus", 'PARTIAL'::"CodeIntelIndexStatus")
		    AND li.status = 'READY'::"CodeIntelIndexStatus"
		    AND o.role IN ('REFERENCE'::"CodeIntelOccurrenceRole", 'IMPORT'::"CodeIntelOccurrenceRole", 'READ'::"CodeIntelOccurrenceRole")
		    AND o.symbol IS NOT NULL
		    AND o.symbol <> ''
		    AND (o.role = 'IMPORT'::"CodeIntelOccurrenceRole" OR COALESCE(o."enclosingSymbol", '') <> '')
		  ORDER BY o."filePath"
		  LIMIT $8
		)
		SELECT o.symbol, o."filePath", o."startLine", o."endLine",
		       o.role::text AS role_text, COALESCE(o."syntaxKind", '') AS syntax_kind,
		       COALESCE(o."lineContent", '') AS line_content,
		       COALESCE(o."enclosingSymbol", '') AS enclosing_symbol
		FROM "CodeIntelOccurrence" o
		JOIN "CodeIntelIndex" ci
		  ON ci.id = o."codeIntelIndexId"
		 AND ci."orgId" = o."orgId"
		 AND ci."repoId" = o."repoId"
		JOIN "CodeIntelLanguageIndex" li
		  ON li.id = o."codeIntelLanguageIndexId"
		 AND li."codeIntelIndexId" = ci.id
		JOIN candidate_files cf
		  ON cf."filePath" = o."filePath"
		WHERE ci."orgId" = $1
		  AND ci."repoId" = $2
		  AND ci."workspaceId" = $3
		  AND ci.branch = $4
		  AND ci.revision = $5
		  AND ci."commitHash" = $6
		  AND ci.kind = 'SCIP'::"CodeIntelIndexKind"
		  AND ci.status IN ('READY'::"CodeIntelIndexStatus", 'PARTIAL'::"CodeIntelIndexStatus")
		  AND li.status = 'READY'::"CodeIntelIndexStatus"
		  AND o.role IN ('REFERENCE'::"CodeIntelOccurrenceRole", 'IMPORT'::"CodeIntelOccurrenceRole", 'READ'::"CodeIntelOccurrenceRole")
		  AND o.symbol IS NOT NULL
		  AND o.symbol <> ''
		  AND (o.role = 'IMPORT'::"CodeIntelOccurrenceRole" OR COALESCE(o."enclosingSymbol", '') <> '')
		ORDER BY
		  CASE o.role::text WHEN 'IMPORT' THEN 0 WHEN 'REFERENCE' THEN 1 ELSE 2 END,
		  o."filePath", o."startLine" NULLS LAST, o."startCharacter" NULLS LAST, o.symbol, o.role::text
		LIMIT $7
	`, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Branch, payload.Revision, payload.CommitHash, scipOccurrenceCap(), scipFileCap())
	if err != nil {
		return nil, nil, nil, err
	}
	defer occurrenceRows.Close()
	var occurrences []scipOccurrenceProjection
	for occurrenceRows.Next() {
		var row scipOccurrenceProjection
		if err := occurrenceRows.Scan(&row.Symbol, &row.FilePath, &row.StartLine, &row.EndLine, &row.Role, &row.SyntaxKind, &row.LineContent, &row.EnclosingSymbol); err != nil {
			return nil, nil, nil, fmt.Errorf("scan SCIP occurrences for graph projection: %w", err)
		}
		occurrences = append(occurrences, row)
	}
	if err := occurrenceRows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("SCIP occurrence projection rows: %w", err)
	}
	h.logger.Info("loaded SCIP occurrence projection rows",
		"repoIndexingJobId", payload.RepoIndexingJobID,
		"orgId", payload.OrgID,
		"repoId", payload.RepoID,
		"count", len(occurrences),
		"durationMs", time.Since(start).Milliseconds(),
	)
	return symbols, relationships, occurrences, nil
}

type rowQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (h *Handler) dbQuery(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q, ok := h.db.(rowQuerier)
	if !ok {
		return nil, errors.New("indexcore: database does not support Query")
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query SCIP graph projection: %w", err)
	}
	return rows, nil
}

func (h *Handler) buildSCIPSnapshot(payload indexsubjobtask.Payload, symbols []scipSymbolProjection, relationships []scipRelationshipProjection, occurrences []scipOccurrenceProjection) graphschema.CodeGraphSnapshot {
	seenVertices := map[string]graphschema.CodeGraphVertex{}
	for _, symbol := range symbols {
		vid := codeGraphVID(payload, "symbol", symbol.Symbol)
		seenVertices[vid] = graphschema.CodeGraphVertex{
			VID:  vid,
			Kind: "symbol",
			Properties: scopedProps(payload, map[string]graphschema.CodeGraphPrimitive{
				"kind":             "symbol",
				"key":              symbol.Symbol,
				"label":            nonEmpty(symbol.DisplayName, symbol.Symbol),
				"symbol":           symbol.Symbol,
				"symbolKind":       symbol.Kind,
				"language":         symbol.Language,
				"path":             symbol.FilePath,
				"evidenceFilePath": symbol.FilePath,
				"startLine":        nullableOneBasedLine(symbol.StartLine),
				"endLine":          nullableOneBasedLine(symbol.EndLine),
				"confidence":       0.95,
				"confidenceTier":   "EXTRACTED",
				"source":           "scip",
				"provenance":       "scip",
			}),
		}
	}
	ensureSymbolVertex := func(symbol string) string {
		vid := codeGraphVID(payload, "symbol", symbol)
		if _, ok := seenVertices[vid]; !ok {
			seenVertices[vid] = graphschema.CodeGraphVertex{
				VID:  vid,
				Kind: "symbol",
				Properties: scopedProps(payload, map[string]graphschema.CodeGraphPrimitive{
					"kind":           "symbol",
					"key":            symbol,
					"label":          symbol,
					"symbol":         symbol,
					"confidence":     0.9,
					"confidenceTier": "EXTRACTED",
					"source":         "scip",
					"provenance":     "scip",
				}),
			}
		}
		return vid
	}
	ensureFileVertex := func(path string) string {
		vid := codeGraphVID(payload, "file", path)
		if _, ok := seenVertices[vid]; !ok {
			seenVertices[vid] = graphschema.CodeGraphVertex{
				VID:  vid,
				Kind: "file",
				Properties: scopedProps(payload, map[string]graphschema.CodeGraphPrimitive{
					"kind":             "file",
					"key":              path,
					"label":            path,
					"path":             path,
					"evidenceFilePath": path,
					"confidence":       0.95,
					"confidenceTier":   "EXTRACTED",
					"source":           "scip",
					"provenance":       "scip",
				}),
			}
		}
		return vid
	}

	var edges []graphschema.CodeGraphEdge
	seenEdges := map[string]struct{}{}
	addEdge := func(fromVID, toVID, kind, context, sourceFile string, startLine, endLine sql.NullInt32, confidence float64, normalizedKey string) {
		if fromVID == "" || toVID == "" || kind == "" {
			return
		}
		// The rendered-statement validator recomputes Nebula edge ranks
		// from from/to/kind/source. Keep the in-memory dedupe key aligned
		// with that identity so repeated occurrence evidence cannot render
		// conflicting parallel edge rows with the same canonical rank.
		key := fromVID + "\x00" + toVID + "\x00" + kind + "\x00scip"
		if _, ok := seenEdges[key]; ok {
			return
		}
		seenEdges[key] = struct{}{}
		edges = append(edges, graphschema.CodeGraphEdge{
			FromVID: fromVID,
			ToVID:   toVID,
			Kind:    kind,
			Rank:    graphschema.EdgeRank(fromVID + "->" + toVID + ":" + kind + ":scip"),
			Properties: scopedProps(payload, map[string]graphschema.CodeGraphPrimitive{
				"kind":             kind,
				"normalizedKey":    normalizedKey,
				"evidenceFilePath": sourceFile,
				"startLine":        nullableOneBasedLine(startLine),
				"endLine":          nullableOneBasedLine(endLine),
				"confidence":       confidence,
				"confidenceTier":   "EXTRACTED",
				"source":           "scip",
				"provenance":       "scip",
				"context":          context,
			}),
		})
	}
	for _, rel := range relationships {
		fromVID := ensureSymbolVertex(rel.SourceSymbol)
		toVID := ensureSymbolVertex(rel.TargetSymbol)
		for _, item := range scipRelationKinds(rel) {
			addEdge(fromVID, toVID, item.kind, item.context, rel.SourceFile, rel.StartLine, rel.EndLine, 0.95, rel.SourceSymbol+"->"+rel.TargetSymbol)
		}
	}
	for _, occ := range occurrences {
		targetVID := ensureSymbolVertex(occ.Symbol)
		switch strings.ToUpper(strings.TrimSpace(occ.Role)) {
		case "IMPORT":
			sourcePath := strings.TrimSpace(occ.FilePath)
			if sourcePath == "" {
				continue
			}
			addEdge(ensureFileVertex(sourcePath), targetVID, "IMPORTS", "import", sourcePath, occ.StartLine, occ.EndLine, 0.95, sourcePath+" imports "+formatSCIPSymbolForEvidence(occ.Symbol))
		case "REFERENCE", "READ":
			if looksLikeImportOccurrence(occ) {
				sourcePath := strings.TrimSpace(occ.FilePath)
				if sourcePath != "" {
					addEdge(ensureFileVertex(sourcePath), targetVID, "IMPORTS", "import", sourcePath, occ.StartLine, occ.EndLine, 0.95, sourcePath+" imports "+formatSCIPSymbolForEvidence(occ.Symbol))
				}
				continue
			}
			sourceSymbol := strings.TrimSpace(occ.EnclosingSymbol)
			if sourceSymbol == "" || sourceSymbol == occ.Symbol {
				continue
			}
			kind, context := scipOccurrenceReferenceKind(occ)
			addEdge(ensureSymbolVertex(sourceSymbol), targetVID, kind, context, occ.FilePath, occ.StartLine, occ.EndLine, 0.95, formatSCIPSymbolForEvidence(sourceSymbol)+" "+kind+" "+formatSCIPSymbolForEvidence(occ.Symbol))
		}
	}

	vertices := make([]graphschema.CodeGraphVertex, 0, len(seenVertices))
	for _, vertex := range seenVertices {
		vertices = append(vertices, vertex)
	}
	return graphschema.CodeGraphSnapshot{
		OrgID:          int64(payload.OrgID),
		WorkspaceID:    *payload.WorkspaceID,
		RepoID:         int64(payload.RepoID),
		Revision:       payload.Revision,
		CommitHash:     payload.CommitHash,
		SchemaVersion:  int64(defaultSchemaVersion),
		BuilderVersion: defaultBuilder,
		Vertices:       vertices,
		Edges:          edges,
	}
}

func (h *Handler) writeSCIPSnapshotToGraph(ctx context.Context, payload indexsubjobtask.Payload, snapshot graphschema.CodeGraphSnapshot) error {
	batchSize := scipGraphBatchSize()
	for start := 0; start < len(snapshot.Vertices); start += batchSize {
		end := start + batchSize
		if end > len(snapshot.Vertices) {
			end = len(snapshot.Vertices)
		}
		chunk := snapshot
		chunk.Vertices = snapshot.Vertices[start:end]
		chunk.Edges = nil
		if err := h.writeSCIPSnapshotChunkToGraph(ctx, payload, chunk); err != nil {
			return err
		}
	}
	for start := 0; start < len(snapshot.Edges); start += batchSize {
		end := start + batchSize
		if end > len(snapshot.Edges) {
			end = len(snapshot.Edges)
		}
		chunk := snapshot
		chunk.Vertices = nil
		chunk.Edges = snapshot.Edges[start:end]
		if err := h.writeSCIPSnapshotChunkToGraph(ctx, payload, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) writeSCIPSnapshotChunkToGraph(ctx context.Context, payload indexsubjobtask.Payload, snapshot graphschema.CodeGraphSnapshot) error {
	if len(snapshot.Vertices) == 0 && len(snapshot.Edges) == 0 {
		return nil
	}
	statements := graphschema.RenderSnapshotStatementsWithBatchSize(snapshot, scipGraphBatchSize())
	result, err := h.graph.WriteRenderedStatements(ctx, graphstore.RenderedStatementWrite{
		OrgID:          int64(payload.OrgID),
		WorkspaceID:    *payload.WorkspaceID,
		RepoID:         int64(payload.RepoID),
		Revision:       payload.Revision,
		CommitHash:     payload.CommitHash,
		SchemaVersion:  int64(defaultSchemaVersion),
		BuilderVersion: defaultBuilder,
		Source:         "scip",
		Statements:     statements,
	})
	if err != nil {
		return fmt.Errorf("write SCIP semantic graph to Nebula: %w", err)
	}
	if result.Status != graphschema.WriteStatusReady {
		return fmt.Errorf("write SCIP semantic graph to Nebula status=%s: %s", result.Status, result.ErrorMessage)
	}
	return nil
}

type relationKind struct {
	kind    string
	context string
}

func scipRelationKinds(rel scipRelationshipProjection) []relationKind {
	var out []relationKind
	if rel.IsReference {
		out = append(out, relationKind{kind: "REFERENCES", context: "reference"})
	}
	if rel.IsImplementation {
		out = append(out, relationKind{kind: "IMPLEMENTS", context: "inherits"})
	}
	if rel.IsTypeDefinition {
		out = append(out, relationKind{kind: "TYPE_DEFINES", context: "type"})
	}
	if rel.IsDefinition {
		out = append(out, relationKind{kind: "DEFINES", context: "definition"})
	}
	return out
}

func scipOccurrenceReferenceKind(occ scipOccurrenceProjection) (string, string) {
	syntaxKind := strings.ToLower(strings.TrimSpace(occ.SyntaxKind))
	line := strings.TrimSpace(occ.LineContent)
	symbolName := scipDisplayNameFromSymbol(occ.Symbol)
	if strings.Contains(syntaxKind, "function") || looksLikeCallExpression(line, symbolName) {
		return "CALLS", "call"
	}
	return "REFERENCES", "reference"
}

func looksLikeCallExpression(line, symbolName string) bool {
	if symbolName == "" || line == "" {
		return false
	}
	idx := strings.Index(line, symbolName)
	for idx >= 0 {
		after := line[idx+len(symbolName):]
		if strings.HasPrefix(strings.TrimLeft(after, " \t"), "(") {
			return true
		}
		next := strings.Index(after, symbolName)
		if next < 0 {
			break
		}
		idx += len(symbolName) + next
	}
	return false
}

func looksLikeImportOccurrence(occ scipOccurrenceProjection) bool {
	line := strings.TrimSpace(occ.LineContent)
	if line == "" {
		return false
	}
	lower := strings.ToLower(line)
	if strings.HasPrefix(lower, "import ") || strings.HasPrefix(lower, "from ") || strings.HasPrefix(lower, "require(") || strings.Contains(lower, " require(") {
		return true
	}
	if strings.Contains(line, "\"") && !strings.Contains(line, "(") && !strings.Contains(line, "=") {
		symbol := strings.ToLower(occ.Symbol)
		return strings.Contains(symbol, " gomod ") || strings.Contains(symbol, " npm ") || strings.Contains(symbol, " pypi ") || strings.Contains(symbol, " maven ") || strings.Contains(line, " ")
	}
	return false
}

func formatSCIPSymbolForEvidence(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return ""
	}
	if quoted := scipQuotedSymbolRE.FindStringSubmatch(symbol); len(quoted) == 2 {
		base := quoted[1]
		suffix := strings.TrimSpace(symbol[strings.LastIndex(symbol, "`")+1:])
		if suffix != "" && suffix != "/" {
			return base + suffix
		}
		return base
	}
	return scipDisplayNameFromSymbol(symbol)
}

func scipDisplayNameFromSymbol(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return ""
	}
	end := len(symbol)
	for end > 0 && (symbol[end-1] == '.' || symbol[end-1] == '#' || symbol[end-1] == ')' || symbol[end-1] == '(' || symbol[end-1] == ']') {
		end--
	}
	start := end
	for start > 0 {
		switch symbol[start-1] {
		case '/', '#', '.', ' ', '(', '[', ':':
			return symbol[start:end]
		default:
			start--
		}
	}
	return symbol[:end]
}

func scopedProps(payload indexsubjobtask.Payload, props map[string]graphschema.CodeGraphPrimitive) map[string]graphschema.CodeGraphPrimitive {
	out := map[string]graphschema.CodeGraphPrimitive{
		"orgId":          int64(payload.OrgID),
		"workspaceId":    *payload.WorkspaceID,
		"repoId":         int64(payload.RepoID),
		"revision":       payload.Revision,
		"commitHash":     payload.CommitHash,
		"schemaVersion":  int64(defaultSchemaVersion),
		"builderVersion": defaultBuilder,
	}
	for k, v := range props {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		out[k] = v
	}
	return out
}

func nullableOneBasedLine(value sql.NullInt32) graphschema.CodeGraphPrimitive {
	if !value.Valid {
		return nil
	}
	return int64(value.Int32 + 1)
}

func scipGraphBatchSize() int {
	raw := strings.TrimSpace(os.Getenv("CODEINTEL_SCIP_GRAPH_NGQL_BATCH_SIZE"))
	if raw == "" {
		return defaultSCIPGraphBatch
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return defaultSCIPGraphBatch
	}
	if value > 10000 {
		return 10000
	}
	return value
}

func scipOccurrenceCap() int {
	return boundedPositiveEnv("CODEINTEL_SCIP_GRAPH_OCCURRENCE_LIMIT", defaultSCIPOccurrenceCap, 1000, 250000)
}

func scipSymbolRoleCap() int {
	return boundedPositiveEnv("CODEINTEL_SCIP_GRAPH_SYMBOL_ROLE_LIMIT", defaultSCIPSymbolRoleCap, 8, 2000)
}

func scipFileCap() int {
	return boundedPositiveEnv("CODEINTEL_SCIP_GRAPH_FILE_LIMIT", defaultSCIPFileCap, 50, 20000)
}

func boundedPositiveEnv(name string, fallback, minValue, maxValue int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func (h *Handler) insertSCIPSemanticFacts(ctx context.Context, graphIndexID string, payload indexsubjobtask.Payload, vertices []graphschema.CodeGraphVertex) error {
	for start := 0; start < len(vertices); start += scipPostgresBatchSize {
		end := start + scipPostgresBatchSize
		if end > len(vertices) {
			end = len(vertices)
		}
		if err := h.insertSCIPSemanticFactBatch(ctx, graphIndexID, payload, vertices[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) insertSCIPSemanticFactBatch(ctx context.Context, graphIndexID string, payload indexsubjobtask.Payload, vertices []graphschema.CodeGraphVertex) error {
	if len(vertices) == 0 {
		return nil
	}
	args := make([]any, 0, len(vertices)*17)
	values := make([]string, 0, len(vertices))
	for _, vertex := range vertices {
		props := vertex.Properties
		dedupeKey := "scip-node:" + vertex.VID
		base := len(args) + 1
		values = append(values, fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, 'NEBULA', $%d, $%d, $%d, $%d, $%d, $%d, NULL, 'EXTRACTED'::\"CodeGraphFactConfidenceTier\", $%d, 'scip', 'scip-projection', NULL, $%d, $%d, $%d, CURRENT_TIMESTAMP)",
			base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10, base+11, base+12, base+13, base+14, base+15, base+16,
		))
		args = append(args,
			h.newID(),
			vertex.VID,
			dedupeKey,
			stringProp(props, "kind", vertex.Kind),
			stringProp(props, "label", vertex.VID),
			*payload.WorkspaceID,
			payload.CommitHash,
			defaultSchemaVersion,
			defaultBuilder,
			stringProp(props, "evidenceFilePath", stringProp(props, "path", "")),
			optionalPropInt(props["startLine"]),
			optionalPropInt(props["endLine"]),
			stringProp(props, "label", vertex.VID),
			0.95,
			payload.OrgID,
			payload.RepoID,
			graphIndexID,
		)
	}
	_, err := h.db.Exec(ctx, `
		INSERT INTO "CodeGraphSemanticFact" (
			id, "externalId", "dedupeKey", kind, label, "workspaceId", "commitHash",
			provider, "schemaVersion", "builderVersion", "sourceFile", "startLine",
			"endLine", evidence, "evidenceHash", "confidenceTier", confidence, source,
			"extractionMethod", "episodeId", "orgId", "repoId", "graphIndexId", "updatedAt"
		) VALUES `+strings.Join(values, ", ")+`
		ON CONFLICT ("graphIndexId", "dedupeKey") DO UPDATE SET
			kind = EXCLUDED.kind,
			label = EXCLUDED.label,
			"sourceFile" = EXCLUDED."sourceFile",
			"startLine" = EXCLUDED."startLine",
			"endLine" = EXCLUDED."endLine",
			evidence = EXCLUDED.evidence,
			"confidenceTier" = EXCLUDED."confidenceTier",
			confidence = EXCLUDED.confidence,
			source = EXCLUDED.source,
			"updatedAt" = CURRENT_TIMESTAMP
	`, args...)
	if err != nil {
		return fmt.Errorf("insert SCIP semantic fact batch: %w", err)
	}
	return nil
}

func (h *Handler) insertSCIPSemanticEdges(ctx context.Context, graphIndexID string, payload indexsubjobtask.Payload, edges []graphschema.CodeGraphEdge) error {
	for start := 0; start < len(edges); start += scipPostgresBatchSize {
		end := start + scipPostgresBatchSize
		if end > len(edges) {
			end = len(edges)
		}
		if err := h.insertSCIPSemanticEdgeBatch(ctx, graphIndexID, payload, edges[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) insertSCIPSemanticEdgeBatch(ctx context.Context, graphIndexID string, payload indexsubjobtask.Payload, edges []graphschema.CodeGraphEdge) error {
	if len(edges) == 0 {
		return nil
	}
	args := make([]any, 0, len(edges)*18)
	values := make([]string, 0, len(edges))
	for _, edge := range edges {
		props := edge.Properties
		dedupeKey := scipEdgeDedupeKey(edge.FromVID, edge.ToVID, edge.Kind, edge.Rank)
		base := len(args) + 1
		values = append(values, fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, 'NEBULA', $%d, $%d, $%d, $%d, $%d, $%d, NULL, $%d, 'EXTRACTED'::\"CodeGraphFactConfidenceTier\", $%d, 'scip', 'scip-projection', NULL, $%d, $%d, $%d, CURRENT_TIMESTAMP)",
			base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10, base+11, base+12, base+13, base+14, base+15, base+16, base+17,
		))
		args = append(args,
			h.newID(),
			edge.FromVID,
			edge.ToVID,
			dedupeKey,
			edge.Kind,
			*payload.WorkspaceID,
			payload.CommitHash,
			defaultSchemaVersion,
			defaultBuilder,
			stringProp(props, "evidenceFilePath", ""),
			optionalPropInt(props["startLine"]),
			optionalPropInt(props["endLine"]),
			stringProp(props, "normalizedKey", ""),
			stringProp(props, "context", ""),
			0.95,
			payload.OrgID,
			payload.RepoID,
			graphIndexID,
		)
	}
	_, err := h.db.Exec(ctx, `
		INSERT INTO "CodeGraphSemanticEdge" (
			id, "sourceExternalId", "targetExternalId", "dedupeKey", relation,
			"workspaceId", "commitHash", provider, "schemaVersion", "builderVersion",
			"sourceFile", "startLine", "endLine", evidence, "evidenceHash", rationale,
			"confidenceTier", confidence, source, "extractionMethod", "episodeId",
			"orgId", "repoId", "graphIndexId", "updatedAt"
		) VALUES `+strings.Join(values, ", ")+`
		ON CONFLICT ("graphIndexId", "dedupeKey") DO UPDATE SET
			relation = EXCLUDED.relation,
			"sourceFile" = EXCLUDED."sourceFile",
			"startLine" = EXCLUDED."startLine",
			"endLine" = EXCLUDED."endLine",
			evidence = EXCLUDED.evidence,
			rationale = EXCLUDED.rationale,
			"confidenceTier" = EXCLUDED."confidenceTier",
			confidence = EXCLUDED.confidence,
			source = EXCLUDED.source,
			"updatedAt" = CURRENT_TIMESTAMP
	`, args...)
	if err != nil {
		return fmt.Errorf("insert SCIP semantic edge batch: %w", err)
	}
	return nil
}

func (h *Handler) insertSCIPSemanticFact(ctx context.Context, graphIndexID string, payload indexsubjobtask.Payload, vertex graphschema.CodeGraphVertex) error {
	props := vertex.Properties
	dedupeKey := "scip-node:" + vertex.VID
	_, err := h.db.Exec(ctx, `
		INSERT INTO "CodeGraphSemanticFact" (
			id, "externalId", "dedupeKey", kind, label, "workspaceId", "commitHash",
			provider, "schemaVersion", "builderVersion", "sourceFile", "startLine",
			"endLine", evidence, "evidenceHash", "confidenceTier", confidence, source,
			"extractionMethod", "episodeId", "orgId", "repoId", "graphIndexId", "updatedAt"
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			'NEBULA', $8, $9, $10, $11,
			$12, $13, NULL, 'EXTRACTED'::"CodeGraphFactConfidenceTier", $14, 'scip',
			'scip-projection', NULL, $15, $16, $17, CURRENT_TIMESTAMP
		)
		ON CONFLICT ("graphIndexId", "dedupeKey") DO UPDATE SET
			kind = EXCLUDED.kind,
			label = EXCLUDED.label,
			"sourceFile" = EXCLUDED."sourceFile",
			"startLine" = EXCLUDED."startLine",
			"endLine" = EXCLUDED."endLine",
			evidence = EXCLUDED.evidence,
			"confidenceTier" = EXCLUDED."confidenceTier",
			confidence = EXCLUDED.confidence,
			source = EXCLUDED.source,
			"updatedAt" = CURRENT_TIMESTAMP
	`, h.newID(), vertex.VID, dedupeKey, stringProp(props, "kind", vertex.Kind),
		stringProp(props, "label", vertex.VID), *payload.WorkspaceID, payload.CommitHash,
		defaultSchemaVersion, defaultBuilder, stringProp(props, "evidenceFilePath", stringProp(props, "path", "")),
		optionalPropInt(props["startLine"]), optionalPropInt(props["endLine"]),
		stringProp(props, "label", vertex.VID), 0.95, payload.OrgID, payload.RepoID, graphIndexID)
	if err != nil {
		return fmt.Errorf("insert SCIP semantic fact: %w", err)
	}
	return nil
}

func (h *Handler) insertSCIPSemanticEdge(ctx context.Context, graphIndexID string, payload indexsubjobtask.Payload, edge graphschema.CodeGraphEdge) error {
	props := edge.Properties
	dedupeKey := scipEdgeDedupeKey(edge.FromVID, edge.ToVID, edge.Kind, edge.Rank)
	_, err := h.db.Exec(ctx, `
		INSERT INTO "CodeGraphSemanticEdge" (
			id, "sourceExternalId", "targetExternalId", "dedupeKey", relation,
			"workspaceId", "commitHash", provider, "schemaVersion", "builderVersion",
			"sourceFile", "startLine", "endLine", evidence, "evidenceHash", rationale,
			"confidenceTier", confidence, source, "extractionMethod", "episodeId",
			"orgId", "repoId", "graphIndexId", "updatedAt"
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, 'NEBULA', $8, $9,
			$10, $11, $12, $13, NULL, $14,
			'EXTRACTED'::"CodeGraphFactConfidenceTier", $15, 'scip', 'scip-projection', NULL,
			$16, $17, $18, CURRENT_TIMESTAMP
		)
		ON CONFLICT ("graphIndexId", "dedupeKey") DO UPDATE SET
			relation = EXCLUDED.relation,
			"sourceFile" = EXCLUDED."sourceFile",
			"startLine" = EXCLUDED."startLine",
			"endLine" = EXCLUDED."endLine",
			evidence = EXCLUDED.evidence,
			rationale = EXCLUDED.rationale,
			"confidenceTier" = EXCLUDED."confidenceTier",
			confidence = EXCLUDED.confidence,
			source = EXCLUDED.source,
			"updatedAt" = CURRENT_TIMESTAMP
	`, h.newID(), edge.FromVID, edge.ToVID, dedupeKey, edge.Kind,
		*payload.WorkspaceID, payload.CommitHash, defaultSchemaVersion, defaultBuilder,
		stringProp(props, "evidenceFilePath", ""), optionalPropInt(props["startLine"]), optionalPropInt(props["endLine"]),
		stringProp(props, "normalizedKey", ""), stringProp(props, "context", ""), 0.95,
		payload.OrgID, payload.RepoID, graphIndexID)
	if err != nil {
		return fmt.Errorf("insert SCIP semantic edge: %w", err)
	}
	return nil
}

func codeGraphVID(payload indexsubjobtask.Payload, kind, key string) string {
	workspaceID := ""
	if payload.WorkspaceID != nil {
		workspaceID = *payload.WorkspaceID
	}
	builderHash := hashParts([]string{defaultBuilder}, 8)
	keyHash := hashParts([]string{
		fmt.Sprint(payload.OrgID),
		workspaceID,
		fmt.Sprint(payload.RepoID),
		payload.CommitHash,
		fmt.Sprint(defaultSchemaVersion),
		defaultBuilder,
		kind,
		key,
	}, 32)
	return fmt.Sprintf("cg:o%d:w%s:r%d:c%s:s%d:b%s:%s:%s",
		payload.OrgID,
		hashParts([]string{workspaceID}, 8),
		payload.RepoID,
		payload.CommitHash[:12],
		defaultSchemaVersion,
		builderHash,
		kind,
		keyHash,
	)
}

func hashParts(parts []string, n int) string {
	h := sha256.New()
	for i, part := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(part))
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:n]
}

func nullableInt32(value sql.NullInt32) graphschema.CodeGraphPrimitive {
	if value.Valid {
		return int64(value.Int32)
	}
	return nil
}

func optionalPropInt(value graphschema.CodeGraphPrimitive) any {
	switch v := value.(type) {
	case int:
		return v
	case int32:
		return v
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return nil
	}
}

func stringProp(props map[string]graphschema.CodeGraphPrimitive, key, fallback string) string {
	value, ok := props[key]
	if !ok || value == nil {
		return fallback
	}
	switch v := value.(type) {
	case string:
		return nonEmpty(v, fallback)
	default:
		return fmt.Sprint(v)
	}
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func scipEdgeDedupeKey(fromVID, toVID, relation string, rank int64) string {
	sum := sha256.Sum256([]byte(fromVID + "\x00" + toVID + "\x00" + relation + "\x00" + strconv.FormatInt(rank, 10)))
	return "scip-edge:" + hex.EncodeToString(sum[:])
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func (h *Handler) activate(ctx context.Context, payload indexsubjobtask.Payload) error {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin activation: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// Serialize activation per repo before touching manifests,
	// graph revisions, or repo metadata. Multi-branch indexes can
	// otherwise let several ACTIVATE subjobs update different
	// tables and then deadlock when they all reach the Repo row.
	var repoLock int
	if err := tx.QueryRow(ctx, `
		SELECT 1
		FROM "Repo"
		WHERE id = $1
		  AND "orgId" = $2
		FOR UPDATE
	`, payload.RepoID, payload.OrgID).Scan(&repoLock); err != nil {
		return fmt.Errorf("lock repo indexed metadata: %w", err)
	}

	var manifestID string
	var providerConnectionID sql.NullString
	if err := tx.QueryRow(ctx, `
		SELECT id, "providerConnectionId"
		FROM "RepoIndexManifest"
		WHERE "indexJobId" = $1
		  AND "orgId" = $2
		  AND "repoId" = $3
		  AND "workspaceId" = $4
		  AND branch = $5
		  AND "commitHash" = $6
		  AND status = 'PENDING'::"RepoIndexManifestStatus"
		ORDER BY "createdAt" DESC, id DESC
		LIMIT 1
	`, payload.RepoIndexingJobID, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Branch, payload.CommitHash).Scan(&manifestID, &providerConnectionID); err != nil {
		return fmt.Errorf("resolve pending manifest: %w", err)
	}

	var graphIndexID string
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM "CodeGraphIndex"
		WHERE "orgId" = $1
		  AND "repoId" = $2
		  AND "workspaceId" = $3
		  AND "sourceRevision" = $4
		  AND "commitHash" = $5
		  AND provider = 'NEBULA'
		  AND "schemaVersion" = $6
		  AND "builderVersion" = $7
		  AND status = 'READY'::"CodeGraphIndexStatus"
		ORDER BY "updatedAt" DESC, id DESC
		LIMIT 1
	`, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Revision, payload.CommitHash, defaultSchemaVersion, defaultBuilder).Scan(&graphIndexID); err != nil {
		return fmt.Errorf("resolve ready graph snapshot: %w", err)
	}

	var blocked int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM "CodeIntelIndexSubjob" dep
		WHERE dep."repoIndexingJobId" = $1
		  AND dep."workspaceId" = $2
		  AND dep."repoId" = $3
		  AND dep.branch = $4
		  AND dep.revision = $5
		  AND dep."commitHash" = $6
		  AND dep.layer <> 'ACTIVATE'
		  AND dep.status NOT IN ('SUCCEEDED', 'SKIPPED')
		  AND NOT (
		    dep.layer = 'SCIP'
		    AND dep.status IN ('FAILED', 'CANCELED')
		  )
	`, payload.RepoIndexingJobID, *payload.WorkspaceID, payload.RepoID, payload.Branch, payload.Revision, payload.CommitHash).Scan(&blocked); err != nil {
		return fmt.Errorf("check activation dependencies: %w", err)
	}
	if blocked != 0 {
		return fmt.Errorf("indexcore: activation blocked by %d unfinished subjobs", blocked)
	}

	var providerArg any
	if providerConnectionID.Valid {
		providerArg = providerConnectionID.String
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "RepoIndexManifest"
		SET status = 'SUPERSEDED'::"RepoIndexManifestStatus",
		    "supersededAt" = CURRENT_TIMESTAMP,
		    "updatedAt" = CURRENT_TIMESTAMP
		WHERE id <> $1
		  AND "orgId" = $2
		  AND "repoId" = $3
		  AND "workspaceId" = $4
		  AND "providerConnectionId" IS NOT DISTINCT FROM $5
		  AND branch = $6
		  AND status = 'READY'::"RepoIndexManifestStatus"
	`, manifestID, payload.OrgID, payload.RepoID, *payload.WorkspaceID, providerArg, payload.Branch); err != nil {
		return fmt.Errorf("supersede previous manifests: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		WITH current_manifest AS (
		    SELECT "createdAt"
		    FROM "RepoIndexManifest"
		    WHERE id = $1
		      AND "orgId" = $2
		      AND "repoId" = $3
		      AND "workspaceId" = $4
		      AND "providerConnectionId" IS NOT DISTINCT FROM $5
		      AND branch = $6
		)
		UPDATE "RepoIndexManifest" stale
		SET status = 'SUPERSEDED'::"RepoIndexManifestStatus",
		    "supersededAt" = CURRENT_TIMESTAMP,
		    "updatedAt" = CURRENT_TIMESTAMP,
		    "errorMessage" = COALESCE(stale."errorMessage", 'Superseded by a newer activated index manifest for the same branch.')
		FROM current_manifest
		WHERE stale.id <> $1
		  AND stale."orgId" = $2
		  AND stale."repoId" = $3
		  AND stale."workspaceId" = $4
		  AND stale."providerConnectionId" IS NOT DISTINCT FROM $5
		  AND stale.branch = $6
		  AND stale.status = 'PENDING'::"RepoIndexManifestStatus"
		  AND stale."createdAt" <= current_manifest."createdAt"
	`, manifestID, payload.OrgID, payload.RepoID, *payload.WorkspaceID, providerArg, payload.Branch); err != nil {
		return fmt.Errorf("supersede stale pending manifests: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "RepoIndexManifest"
		SET status = 'READY'::"RepoIndexManifestStatus",
		    "activatedAt" = CURRENT_TIMESTAMP,
		    "failedAt" = NULL,
		    "errorMessage" = NULL,
		    "updatedAt" = CURRENT_TIMESTAMP
		WHERE id = $1
		  AND status = 'PENDING'::"RepoIndexManifestStatus"
	`, manifestID); err != nil {
		return fmt.Errorf("activate manifest: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO "CodeGraphRevision" (
			id, revision, "workspaceId", "commitHash", provider,
			"schemaVersion", "builderVersion", "activatedAt",
			"updatedAt", "orgId", "repoId", "codeGraphIndexId"
		) VALUES (
			$1, $2, $3, $4, 'NEBULA',
			$5, $6, CURRENT_TIMESTAMP,
			CURRENT_TIMESTAMP, $7, $8, $9
		)
		ON CONFLICT ("repoId", "workspaceId", revision, provider, "schemaVersion", "builderVersion")
		DO UPDATE SET
			"commitHash" = EXCLUDED."commitHash",
			"codeGraphIndexId" = EXCLUDED."codeGraphIndexId",
			"activatedAt" = CURRENT_TIMESTAMP,
			"updatedAt" = CURRENT_TIMESTAMP
	`, h.newID(), payload.Revision, *payload.WorkspaceID, payload.CommitHash,
		defaultSchemaVersion, defaultBuilder, payload.OrgID, payload.RepoID, graphIndexID); err != nil {
		return fmt.Errorf("activate graph revision: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "Repo" r
		SET metadata = jsonb_set(
		        COALESCE(r.metadata, '{}'::jsonb),
		        '{indexedRevisions}',
		        COALESCE((
		            SELECT jsonb_agg(branch ORDER BY branch)
		            FROM (
		                SELECT DISTINCT branch
		                FROM (
		                    SELECT jsonb_array_elements_text(COALESCE(r.metadata->'indexedRevisions', '[]'::jsonb)) AS branch
		                    UNION
		                    SELECT branch
		                    FROM "RepoIndexManifest"
		                    WHERE "orgId" = $2
		                      AND "repoId" = $1
		                      AND "workspaceId" = $3
		                      AND status = 'READY'::"RepoIndexManifestStatus"
		                ) merged
		                WHERE branch <> ''
		            ) ready
		        ), '[]'::jsonb),
		        true
		    ),
		    "indexedAt" = CURRENT_TIMESTAMP,
		    "indexedCommitHash" = $4,
		    "updatedAt" = CURRENT_TIMESTAMP
		WHERE r.id = $1 AND r."orgId" = $2
	`, payload.RepoID, payload.OrgID, *payload.WorkspaceID, payload.CommitHash); err != nil {
		return fmt.Errorf("update repo indexed metadata: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit activation: %w", err)
	}
	return nil
}

func (h *Handler) completeJobIfReady(ctx context.Context, payload indexsubjobtask.Payload) error {
	_, err := h.db.Exec(ctx, `
		UPDATE "RepoIndexingJob" j
		SET status = 'COMPLETED'::"RepoIndexingJobStatus",
		    "completedAt" = CURRENT_TIMESTAMP,
		    "updatedAt" = CURRENT_TIMESTAMP
		WHERE j.id = $1
		  AND j."repoId" = $2
		  AND j.type = 'INDEX'::"RepoIndexingJobType"
		  AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		  AND NOT EXISTS (
		    SELECT 1
		    FROM "CodeIntelIndexSubjob" s
		    WHERE s."repoIndexingJobId" = j.id
		      AND s.status NOT IN ('SUCCEEDED', 'SKIPPED')
		      AND NOT (
		        s.layer = 'SCIP'
		        AND s.status IN ('FAILED', 'CANCELED')
		      )
		  )
	`, payload.RepoIndexingJobID, payload.RepoID)
	if err != nil {
		return fmt.Errorf("complete index job: %w", err)
	}
	if _, err := h.db.Exec(ctx, `
		UPDATE "Repo" r
		SET "latestIndexingJobStatus" = sub.status
		FROM (
		    SELECT j.status
		    FROM "RepoIndexingJob" j
		    WHERE j."repoId" = $1
		    ORDER BY j."createdAt" DESC NULLS LAST, j.id DESC
		    LIMIT 1
		) sub
		WHERE r.id = $1 AND r."orgId" = $2
	`, payload.RepoID, payload.OrgID); err != nil {
		return fmt.Errorf("refresh repo latest indexing job status: %w", err)
	}
	return nil
}

func toClaimScope(p indexsubjobtask.Payload) indexsubjobs.ClaimScope {
	return indexsubjobs.ClaimScope{
		ID:                p.SubjobID,
		RepoIndexingJobID: p.RepoIndexingJobID,
		OrgID:             p.OrgID,
		WorkspaceID:       p.WorkspaceID,
		RepoID:            p.RepoID,
		Branch:            p.Branch,
		Revision:          p.Revision,
		CommitHash:        p.CommitHash,
		Layer:             indexsubjobs.Layer(p.Layer),
		Language:          p.Language,
		ProjectRoot:       p.ProjectRoot,
		Indexer:           p.Indexer,
		WorkerClass:       p.WorkerClass,
		QueueName:         p.QueueName,
		Attempt:           p.Attempt,
	}
}

func generatedAttemptID(subjobID string, attempt int32) string {
	return fmt.Sprintf("%s:%d:%s", subjobID, attempt, nonceHex(12))
}

func nonceHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
