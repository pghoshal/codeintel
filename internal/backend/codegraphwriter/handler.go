// Package codegraphwriter consumes Rust-rendered graph write
// tasks and persists them through the Go-owned Nebula writer.
package codegraphwriter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode"

	"codeintel/internal/backend/graphstore"
	"codeintel/pkg/graphschema"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	maxPostgresInt       = int64(1<<31 - 1)
	maxPayloadBytes      = 16 << 20
	maxStatementCount    = 20_000
	maxStatementBytes    = 1 << 20
	semanticRowBatchSize = 500
)

var errStaleManifest = errors.New("codegraphwriter: manifest is no longer active")

type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type graphWriter interface {
	WriteRenderedStatements(ctx context.Context, input graphstore.RenderedStatementWrite) (graphschema.CodeGraphWriteResult, error)
}

type PayloadAnchor struct {
	Kind             string  `json:"kind"`
	Direction        string  `json:"direction"`
	Key              string  `json:"key"`
	NormalizedKey    string  `json:"normalizedKey"`
	NodeVID          string  `json:"nodeVid"`
	EvidenceFilePath *string `json:"evidenceFilePath"`
	StartLine        *int64  `json:"startLine"`
	EndLine          *int64  `json:"endLine"`
	Confidence       float64 `json:"confidence"`
	ConfidenceTier   string  `json:"confidenceTier"`
	Source           string  `json:"source"`
}

// Payload mirrors the Rust CodeGraphWritePayload JSON contract.
type Payload struct {
	OrgID                int64           `json:"orgId"`
	WorkspaceID          string          `json:"workspaceId"`
	RepoID               int64           `json:"repoId"`
	Branch               string          `json:"branch,omitempty"`
	Revision             string          `json:"revision"`
	CommitHash           string          `json:"commitHash"`
	SchemaVersion        int64           `json:"schemaVersion"`
	BuilderVersion       string          `json:"builderVersion"`
	IndexJobID           string          `json:"indexJobId"`
	ManifestID           string          `json:"manifestId"`
	ProviderConnectionID *string         `json:"providerConnectionId"`
	Source               string          `json:"source"`
	Statements           []string        `json:"statements"`
	Anchors              []PayloadAnchor `json:"anchors,omitempty"`
}

// Handler is the asynq consumer for the code-graph-write queue.
type Handler struct {
	db     pgxQuerier
	graph  graphWriter
	logger *slog.Logger
	newID  func() string
}

func NewHandler(db pgxQuerier, graph graphWriter, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		db:     db,
		graph:  graph,
		logger: logger.With("component", "code-graph-write"),
		newID:  uuid.NewString,
	}
}

func (h *Handler) AsynqHandlerFunc() func(context.Context, *asynq.Task) error {
	return h.Handle
}

func (h *Handler) Handle(ctx context.Context, t *asynq.Task) error {
	if h == nil || h.db == nil || h.graph == nil {
		return errors.New("codegraphwriter: handler not configured")
	}
	payload, err := UnmarshalPayload(t.Payload())
	if err != nil {
		return skipRetry("payload decode: %w", err)
	}
	return h.writePayload(ctx, payload, writeOptions{
		AllowPendingManifest: false,
		ActivateRevision:     true,
	})
}

type writeOptions struct {
	AllowPendingManifest bool
	ActivateRevision     bool
	PartialOnly          bool
}

// WritePendingPayload persists a graph artifact that belongs to an
// in-progress split-index subjob. The manifest is allowed to remain
// PENDING because the later ACTIVATE layer owns RepoIndexManifest READY
// transition and CodeGraphRevision activation.
func (h *Handler) WritePendingPayload(ctx context.Context, payload Payload) error {
	return h.writePayload(ctx, payload, writeOptions{
		AllowPendingManifest: true,
		ActivateRevision:     false,
		PartialOnly:          true,
	})
}

func (h *Handler) writePayload(ctx context.Context, payload Payload, opts writeOptions) error {
	if h == nil || h.db == nil || h.graph == nil {
		return errors.New("codegraphwriter: handler not configured")
	}
	if err := payload.Validate(); err != nil {
		return skipRetry("payload validation: %w", err)
	}
	renderedInput := renderedInputFromPayload(payload)
	if _, _, err := graphstore.ValidateRenderedStatementWrite(renderedInput); err != nil {
		return skipRetry("rendered statement validation: %w", err)
	}
	scope, err := h.resolveScope(ctx, payload)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return skipRetry("scope validation: repo %d does not belong to org %d", payload.RepoID, payload.OrgID)
		}
		return fmt.Errorf("scope validation: %w", err)
	}
	if scope.WorkspaceID != payload.WorkspaceID {
		return skipRetry("scope validation: workspaceId %q does not match org-resolved workspaceId %q", payload.WorkspaceID, scope.WorkspaceID)
	}

	manifestStatus, err := h.manifestStatus(ctx, payload)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return skipRetry("manifest validation: no manifest for repo=%d revision=%q commit=%s", payload.RepoID, payload.Revision, payload.CommitHash)
		}
		return fmt.Errorf("manifest validation: %w", err)
	}
	switch manifestStatus {
	case "READY":
	case "PENDING":
		if !opts.AllowPendingManifest {
			return fmt.Errorf("manifest %s/%s is still PENDING; retry graph write later", payload.Revision, payload.CommitHash)
		}
	case "SUPERSEDED", "FAILED":
		return skipRetry("manifest validation: manifest is %s; stale graph write will not be applied", manifestStatus)
	default:
		return skipRetry("manifest validation: unsupported manifest status %q", manifestStatus)
	}

	graphBuild, err := h.markBuilding(ctx, payload)
	if err != nil {
		return fmt.Errorf("mark graph BUILDING: %w", err)
	}
	switch graphBuild.Status {
	case "BUILDING":
	case "PARTIAL":
		// PARTIAL rows are layer artifacts, not final active graphs.
		// Duplicate split writes and future GRAPH_MERGE/ACTIVATE writes
		// must be allowed to rebuild them.
	case "READY":
		if opts.ActivateRevision {
			if err := h.upsertRevisionIfManifestReady(ctx, graphBuild.ID, payload); err != nil {
				if errors.Is(err, errStaleManifest) {
					h.logger.Warn("skipped duplicate graph revision activation because manifest is stale",
						"orgId", payload.OrgID,
						"repoId", payload.RepoID,
						"revision", payload.Revision,
						"commitHash", shortCommit(payload.CommitHash),
					)
					return nil
				}
				return fmt.Errorf("upsert graph revision: %w", err)
			}
		}
		if err := h.repairReadyGraphCounts(ctx, graphBuild.ID, payload); err != nil {
			return fmt.Errorf("repair READY graph metadata: %w", err)
		}
		if !opts.ActivateRevision {
			h.logger.Info("code graph write already READY; duplicate split-index artifact consumed",
				"orgId", payload.OrgID,
				"repoId", payload.RepoID,
				"revision", payload.Revision,
				"commitHash", shortCommit(payload.CommitHash),
			)
			return nil
		}
		h.logger.Info("code graph write already READY; duplicate delivery consumed",
			"orgId", payload.OrgID,
			"repoId", payload.RepoID,
			"revision", payload.Revision,
			"commitHash", shortCommit(payload.CommitHash),
		)
		return nil
	case "SKIPPED":
		h.logger.Info("code graph write already SKIPPED; duplicate delivery consumed",
			"orgId", payload.OrgID,
			"repoId", payload.RepoID,
			"revision", payload.Revision,
			"commitHash", shortCommit(payload.CommitHash),
		)
		return nil
	default:
		return skipRetry("mark graph BUILDING returned unsupported status %q", graphBuild.Status)
	}

	result, err := h.graph.WriteRenderedStatements(ctx, renderedInput)
	if err != nil {
		_ = h.markFailed(ctx, graphBuild.ID, payload, err.Error())
		if errors.Is(err, graphstore.ErrRenderedStatementValidation) {
			return skipRetry("rendered statement validation: %w", err)
		}
		return fmt.Errorf("write rendered graph: %w", err)
	}

	switch result.Status {
	case graphschema.WriteStatusReady:
		summary, err := h.persistRenderedSemanticRows(ctx, graphBuild.ID, payload, renderedInput)
		if err != nil {
			_ = h.markFailed(ctx, graphBuild.ID, payload, err.Error())
			return fmt.Errorf("persist rendered graph semantic rows: %w", err)
		}
		result.AnchorCount = summary.AnchorCount
		result.LinkedEdgeCount = summary.LinkedEdgeCount
		if opts.PartialOnly {
			if err := h.markPartial(ctx, graphBuild.ID, payload, result); err != nil {
				return fmt.Errorf("mark graph PARTIAL: %w", err)
			}
			break
		}
		if err := h.markReady(ctx, graphBuild.ID, payload, result); err != nil {
			return fmt.Errorf("mark graph READY: %w", err)
		}
		if opts.ActivateRevision {
			if err := h.upsertRevisionIfManifestReady(ctx, graphBuild.ID, payload); err != nil {
				if errors.Is(err, errStaleManifest) {
					h.logger.Warn("graph write completed but active revision was not updated because manifest is stale",
						"orgId", payload.OrgID,
						"repoId", payload.RepoID,
						"revision", payload.Revision,
						"commitHash", shortCommit(payload.CommitHash),
					)
					return nil
				}
				return fmt.Errorf("upsert graph revision: %w", err)
			}
		}
	case graphschema.WriteStatusSkipped:
		if err := h.markSkipped(ctx, graphBuild.ID, payload, result.ErrorMessage); err != nil {
			return fmt.Errorf("mark graph SKIPPED: %w", err)
		}
	case graphschema.WriteStatusFailed:
		_ = h.markFailed(ctx, graphBuild.ID, payload, result.ErrorMessage)
		return fmt.Errorf("write rendered graph failed: %s", result.ErrorMessage)
	default:
		_ = h.markFailed(ctx, graphBuild.ID, payload, "unknown graph write status "+string(result.Status))
		return fmt.Errorf("unknown graph write status %q", result.Status)
	}

	h.logger.Info("code graph write completed",
		"orgId", payload.OrgID,
		"repoId", payload.RepoID,
		"revision", payload.Revision,
		"commitHash", shortCommit(payload.CommitHash),
		"status", result.Status,
		"vertices", result.VertexCount,
		"edges", result.EdgeCount,
	)
	return nil
}

func UnmarshalPayload(raw []byte) (Payload, error) {
	if len(raw) > maxPayloadBytes {
		return Payload{}, fmt.Errorf("payload size %d exceeds %d bytes", len(raw), maxPayloadBytes)
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Payload{}, err
	}
	return p, nil
}

func (p Payload) Validate() error {
	if p.OrgID <= 0 || p.OrgID > maxPostgresInt {
		return fmt.Errorf("invalid orgId %d", p.OrgID)
	}
	if p.RepoID <= 0 || p.RepoID > maxPostgresInt {
		return fmt.Errorf("invalid repoId %d", p.RepoID)
	}
	if strings.TrimSpace(p.WorkspaceID) == "" {
		return errors.New("workspaceId is required")
	}
	if strings.TrimSpace(p.Revision) == "" {
		return errors.New("revision is required")
	}
	if strings.TrimSpace(p.Branch) == "" {
		p.Branch = p.Revision
	}
	if !isHexCommit(p.CommitHash) {
		return errors.New("commitHash must be a 40-character SHA")
	}
	if p.SchemaVersion <= 0 || p.SchemaVersion > maxPostgresInt {
		return fmt.Errorf("invalid schemaVersion %d", p.SchemaVersion)
	}
	if strings.TrimSpace(p.BuilderVersion) == "" {
		return errors.New("builderVersion is required")
	}
	if strings.TrimSpace(p.IndexJobID) == "" {
		return errors.New("indexJobId is required")
	}
	if strings.TrimSpace(p.ManifestID) == "" {
		return errors.New("manifestId is required")
	}
	if p.ProviderConnectionID != nil && strings.TrimSpace(*p.ProviderConnectionID) == "" {
		return errors.New("providerConnectionId must be non-empty when present")
	}
	if strings.TrimSpace(p.Source) == "" {
		return errors.New("source is required")
	}
	if len(p.Statements) == 0 {
		return errors.New("statements are required")
	}
	if len(p.Statements) > maxStatementCount {
		return fmt.Errorf("statement count %d exceeds %d", len(p.Statements), maxStatementCount)
	}
	for i, stmt := range p.Statements {
		if len(stmt) > maxStatementBytes {
			return fmt.Errorf("statement %d size %d exceeds %d bytes", i, len(stmt), maxStatementBytes)
		}
	}
	for i, anchor := range p.Anchors {
		if err := anchor.Validate(); err != nil {
			return fmt.Errorf("anchor %d: %w", i, err)
		}
		if !anchorBelongsToPayloadScope(anchor, p) {
			return fmt.Errorf("anchor %d: nodeVid is outside payload org/repo scope", i)
		}
	}
	return nil
}

func renderedInputFromPayload(p Payload) graphstore.RenderedStatementWrite {
	return graphstore.RenderedStatementWrite{
		OrgID:          p.OrgID,
		WorkspaceID:    p.WorkspaceID,
		RepoID:         p.RepoID,
		Revision:       p.Revision,
		CommitHash:     p.CommitHash,
		SchemaVersion:  p.SchemaVersion,
		BuilderVersion: p.BuilderVersion,
		Source:         p.Source,
		Statements:     p.Statements,
	}
}

type renderedPersistenceSummary struct {
	AnchorCount     int64
	LinkedEdgeCount int64
}

func (h *Handler) persistRenderedSemanticRows(ctx context.Context, graphIndexID string, p Payload, input graphstore.RenderedStatementWrite) (renderedPersistenceSummary, error) {
	vertices, edges, err := graphstore.ExtractRenderedRows(input)
	if err != nil {
		return renderedPersistenceSummary{}, err
	}
	if _, err := h.db.Exec(ctx, `DELETE FROM "CodeGraphAnchor" WHERE "graphIndexId" = $1 AND "orgId" = $2 AND "repoId" = $3`, graphIndexID, int32(p.OrgID), int32(p.RepoID)); err != nil {
		return renderedPersistenceSummary{}, err
	}
	if _, err := h.db.Exec(ctx, `DELETE FROM "CodeGraphSemanticEdge" WHERE "graphIndexId" = $1 AND "orgId" = $2 AND "repoId" = $3`, graphIndexID, int32(p.OrgID), int32(p.RepoID)); err != nil {
		return renderedPersistenceSummary{}, err
	}
	if _, err := h.db.Exec(ctx, `DELETE FROM "CodeGraphSemanticFact" WHERE "graphIndexId" = $1 AND "orgId" = $2 AND "repoId" = $3`, graphIndexID, int32(p.OrgID), int32(p.RepoID)); err != nil {
		return renderedPersistenceSummary{}, err
	}
	for start := 0; start < len(p.Anchors); start += semanticRowBatchSize {
		end := start + semanticRowBatchSize
		if end > len(p.Anchors) {
			end = len(p.Anchors)
		}
		if err := h.insertRenderedAnchors(ctx, graphIndexID, p, p.Anchors[start:end]); err != nil {
			return renderedPersistenceSummary{}, err
		}
	}
	for start := 0; start < len(vertices); start += semanticRowBatchSize {
		end := start + semanticRowBatchSize
		if end > len(vertices) {
			end = len(vertices)
		}
		if err := h.insertRenderedSemanticFacts(ctx, graphIndexID, p, vertices[start:end]); err != nil {
			return renderedPersistenceSummary{}, err
		}
	}
	for start := 0; start < len(edges); start += semanticRowBatchSize {
		end := start + semanticRowBatchSize
		if end > len(edges) {
			end = len(edges)
		}
		if err := h.insertRenderedSemanticEdges(ctx, graphIndexID, p, edges[start:end]); err != nil {
			return renderedPersistenceSummary{}, err
		}
	}
	return renderedPersistenceSummary{
		AnchorCount:     int64(len(p.Anchors)),
		LinkedEdgeCount: countRenderedAnchorLinks(edges, p.Anchors),
	}, nil
}

func (a PayloadAnchor) Validate() error {
	if strings.TrimSpace(a.Kind) == "" {
		return errors.New("kind is required")
	}
	switch strings.ToUpper(strings.TrimSpace(a.Direction)) {
	case "PROVIDES", "CONSUMES", "REFERENCES":
	default:
		return fmt.Errorf("invalid direction %q", a.Direction)
	}
	if strings.TrimSpace(a.Key) == "" {
		return errors.New("key is required")
	}
	if strings.TrimSpace(a.NormalizedKey) == "" {
		return errors.New("normalizedKey is required")
	}
	if strings.TrimSpace(a.NodeVID) == "" {
		return errors.New("nodeVid is required")
	}
	if strings.TrimSpace(a.Source) == "" {
		return errors.New("source is required")
	}
	if strings.TrimSpace(a.ConfidenceTier) == "" {
		return errors.New("confidenceTier is required")
	}
	if a.Confidence < 0 || a.Confidence > 1 {
		return fmt.Errorf("confidence %f is outside 0..1", a.Confidence)
	}
	return nil
}

func (h *Handler) insertRenderedAnchors(ctx context.Context, graphIndexID string, p Payload, anchors []PayloadAnchor) error {
	if len(anchors) == 0 {
		return nil
	}
	var sql strings.Builder
	sql.WriteString(`
		INSERT INTO "CodeGraphAnchor" (
			id, kind, direction, key, "normalizedKey", "nodeVid",
			"workspaceId", "commitHash", provider, "schemaVersion", "builderVersion",
			"evidenceFilePath", "startLine", "endLine", confidence, source,
			"orgId", "repoId", "graphIndexId", "updatedAt"
		) VALUES `)
	args := make([]any, 0, len(anchors)*18)
	for i, anchor := range anchors {
		if i > 0 {
			sql.WriteString(",")
		}
		base := i*18 + 1
		sql.WriteString(fmt.Sprintf(`(
			$%d, $%d, $%d::"CodeGraphAnchorDirection", $%d, $%d, $%d,
			$%d, $%d, 'NEBULA', $%d, $%d,
			$%d, $%d, $%d, $%d, $%d,
			$%d, $%d, $%d, CURRENT_TIMESTAMP
		)`, base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8,
			base+9, base+10, base+11, base+12, base+13, base+14, base+15, base+16,
			base+17))
		args = append(args,
			h.newID(), anchor.Kind, strings.ToUpper(strings.TrimSpace(anchor.Direction)),
			anchor.Key, anchor.NormalizedKey, anchor.NodeVID, p.WorkspaceID, p.CommitHash,
			int32(p.SchemaVersion), p.BuilderVersion, nullableStringPtr(anchor.EvidenceFilePath),
			optionalInt64(anchor.StartLine), optionalInt64(anchor.EndLine), anchor.Confidence,
			anchor.Source, int32(p.OrgID), int32(p.RepoID), graphIndexID,
		)
	}
	sql.WriteString(`
		ON CONFLICT ("graphIndexId", kind, direction, "normalizedKey", "nodeVid") DO UPDATE SET
			key = EXCLUDED.key,
			"evidenceFilePath" = EXCLUDED."evidenceFilePath",
			"startLine" = EXCLUDED."startLine",
			"endLine" = EXCLUDED."endLine",
			confidence = EXCLUDED.confidence,
			source = EXCLUDED.source,
			"updatedAt" = CURRENT_TIMESTAMP`)
	_, err := h.db.Exec(ctx, sql.String(), args...)
	return err
}

func (h *Handler) insertRenderedSemanticFacts(ctx context.Context, graphIndexID string, p Payload, rows []graphstore.RenderedVertexRow) error {
	if len(rows) == 0 {
		return nil
	}
	var sql strings.Builder
	sql.WriteString(`
		INSERT INTO "CodeGraphSemanticFact" (
			id, "externalId", "dedupeKey", kind, label, "workspaceId", "commitHash",
			provider, "schemaVersion", "builderVersion", "sourceFile", "startLine",
			"endLine", evidence, "evidenceHash", "confidenceTier", confidence, source,
			"extractionMethod", "episodeId", "orgId", "repoId", "graphIndexId", "updatedAt"
		) VALUES `)
	args := make([]any, 0, len(rows)*19)
	for i, row := range rows {
		if i > 0 {
			sql.WriteString(",")
		}
		base := i*19 + 1
		sql.WriteString(fmt.Sprintf(`(
			$%d, $%d, $%d, $%d, $%d, $%d, $%d,
			'NEBULA', $%d, $%d, $%d, $%d,
			$%d, $%d, NULL, $%d::"CodeGraphFactConfidenceTier", $%d, $%d,
			'rendered-ngql', NULL, $%d, $%d, $%d, CURRENT_TIMESTAMP
		)`, base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8,
			base+9, base+10, base+11, base+12, base+13, base+14, base+15, base+16,
			base+17, base+18))
		kind := nonEmpty(row.Props["kind"], "node")
		label := nonEmpty(row.Props["label"], nonEmpty(row.Props["key"], row.VID))
		sourceFile := nonEmpty(row.Props["path"], row.Props["evidenceFilePath"])
		confidenceTier := normalizeConfidenceTier(row.Props["confidenceTier"])
		confidence := parseConfidence(row.Props["confidence"])
		source := nonEmpty(row.Props["source"], p.Source)
		dedupeKey := "rendered-node:" + row.VID
		args = append(args,
			h.newID(), row.VID, dedupeKey, kind, label, p.WorkspaceID, p.CommitHash,
			int32(p.SchemaVersion), p.BuilderVersion, sourceFile,
			optionalInt(row.Props["startLine"]), optionalInt(row.Props["endLine"]),
			label, confidenceTier, confidence, source, int32(p.OrgID), int32(p.RepoID),
			graphIndexID,
		)
	}
	sql.WriteString(`
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
			"updatedAt" = CURRENT_TIMESTAMP`)
	_, err := h.db.Exec(ctx, sql.String(), args...)
	return err
}

func (h *Handler) insertRenderedSemanticEdges(ctx context.Context, graphIndexID string, p Payload, rows []graphstore.RenderedEdgeRow) error {
	if len(rows) == 0 {
		return nil
	}
	var sql strings.Builder
	sql.WriteString(`
		INSERT INTO "CodeGraphSemanticEdge" (
			id, "sourceExternalId", "targetExternalId", "dedupeKey", relation,
			"workspaceId", "commitHash", provider, "schemaVersion", "builderVersion",
			"sourceFile", "startLine", "endLine", evidence, "evidenceHash", rationale,
			"confidenceTier", confidence, source, "extractionMethod", "episodeId",
			"orgId", "repoId", "graphIndexId", "updatedAt"
		) VALUES `)
	args := make([]any, 0, len(rows)*19)
	for i, row := range rows {
		if i > 0 {
			sql.WriteString(",")
		}
		base := i*19 + 1
		sql.WriteString(fmt.Sprintf(`(
			$%d, $%d, $%d, $%d, $%d,
			$%d, $%d, 'NEBULA', $%d, $%d,
			$%d, $%d, $%d, $%d, NULL, NULL,
			$%d::"CodeGraphFactConfidenceTier", $%d, $%d, 'rendered-ngql', NULL,
			$%d, $%d, $%d, CURRENT_TIMESTAMP
		)`, base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8,
			base+9, base+10, base+11, base+12, base+13, base+14, base+15, base+16,
			base+17, base+18))
		relation := nonEmpty(row.Props["kind"], "RELATES_TO")
		sourceFile := row.Props["evidenceFilePath"]
		confidenceTier := normalizeConfidenceTier(row.Props["confidenceTier"])
		confidence := parseConfidence(row.Props["confidence"])
		source := nonEmpty(row.Props["source"], p.Source)
		evidence := row.Props["normalizedKey"]
		dedupeKey := renderedEdgeDedupeKey(row.FromVID, row.ToVID, relation, row.Rank)
		args = append(args,
			h.newID(), row.FromVID, row.ToVID, dedupeKey, relation,
			p.WorkspaceID, p.CommitHash, int32(p.SchemaVersion), p.BuilderVersion,
			sourceFile, optionalInt(row.Props["startLine"]), optionalInt(row.Props["endLine"]),
			nullableString(evidence), confidenceTier, confidence, source,
			int32(p.OrgID), int32(p.RepoID), graphIndexID,
		)
	}
	sql.WriteString(`
		ON CONFLICT ("graphIndexId", "dedupeKey") DO UPDATE SET
			relation = EXCLUDED.relation,
			"sourceFile" = EXCLUDED."sourceFile",
			"startLine" = EXCLUDED."startLine",
			"endLine" = EXCLUDED."endLine",
			evidence = EXCLUDED.evidence,
			"confidenceTier" = EXCLUDED."confidenceTier",
			confidence = EXCLUDED.confidence,
			source = EXCLUDED.source,
			"updatedAt" = CURRENT_TIMESTAMP`)
	_, err := h.db.Exec(ctx, sql.String(), args...)
	return err
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableStringPtr(value *string) any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return *value
}

func optionalInt(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	i, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return nil
	}
	return int32(i)
}

func optionalInt64(value *int64) any {
	if value == nil {
		return nil
	}
	if *value > int64(^uint32(0)>>1) || *value < -int64(^uint32(0)>>1)-1 {
		return nil
	}
	return int32(*value)
}

func countRenderedAnchorLinks(edges []graphstore.RenderedEdgeRow, anchors []PayloadAnchor) int64 {
	anchoredVIDs := make(map[string]bool, len(anchors))
	for _, anchor := range anchors {
		vid := strings.TrimSpace(anchor.NodeVID)
		if vid != "" {
			anchoredVIDs[vid] = true
		}
	}
	var count int64
	for _, edge := range edges {
		if strings.EqualFold(strings.TrimSpace(edge.Props["source"]), "anchor-linker") {
			count++
			continue
		}
		if anchoredVIDs[edge.FromVID] || anchoredVIDs[edge.ToVID] {
			count++
		}
	}
	return count
}

func anchorBelongsToPayloadScope(anchor PayloadAnchor, p Payload) bool {
	return strings.HasPrefix(anchor.NodeVID, "cg:o"+strconv.FormatInt(p.OrgID, 10)+":") &&
		strings.Contains(anchor.NodeVID, ":r"+strconv.FormatInt(p.RepoID, 10)+":")
}

func parseConfidence(value string) float64 {
	if strings.TrimSpace(value) == "" {
		return 1
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 1
	}
	return f
}

func normalizeConfidenceTier(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "EXTRACTED", "INFERRED", "AMBIGUOUS":
		return strings.ToUpper(strings.TrimSpace(value))
	default:
		return "INFERRED"
	}
}

func renderedEdgeDedupeKey(fromVID, toVID, relation, rank string) string {
	sum := sha256.Sum256([]byte(fromVID + "\x00" + toVID + "\x00" + relation + "\x00" + rank))
	return fmt.Sprintf("rendered-edge:%x", sum[:])
}

type resolvedScope struct {
	WorkspaceID string
}

func (h *Handler) resolveScope(ctx context.Context, p Payload) (resolvedScope, error) {
	var workspaceID string
	err := h.db.QueryRow(ctx, `
		SELECT COALESCE(o."atomWorkspaceId", o.domain, 'org-' || o.id::text) AS "workspaceId"
		FROM "Repo" r
		JOIN "Org" o ON o.id = r."orgId"
		WHERE r.id = $1 AND r."orgId" = $2
	`, int32(p.RepoID), int32(p.OrgID)).Scan(&workspaceID)
	if err != nil {
		return resolvedScope{}, err
	}
	return resolvedScope{WorkspaceID: workspaceID}, nil
}

func (h *Handler) manifestStatus(ctx context.Context, p Payload) (string, error) {
	var status string
	err := h.db.QueryRow(ctx, `
		SELECT status::text
		FROM "RepoIndexManifest"
		WHERE "orgId" = $1
		  AND "repoId" = $2
		  AND "workspaceId" = $3
		  AND branch = $4
		  AND "commitHash" = $5
		  AND id = $6
		  AND "indexJobId" = $7
		  AND (("providerConnectionId" IS NULL AND $8::text IS NULL) OR "providerConnectionId" = $8)
	`, int32(p.OrgID), int32(p.RepoID), p.WorkspaceID, p.manifestBranch(), p.CommitHash,
		p.ManifestID, p.IndexJobID, p.providerConnectionArg()).Scan(&status)
	if err != nil {
		return "", err
	}
	return status, nil
}

func (p Payload) manifestBranch() string {
	if strings.TrimSpace(p.Branch) == "" {
		return p.Revision
	}
	return p.Branch
}

type graphBuildState struct {
	ID     string
	Status string
}

func (h *Handler) markBuilding(ctx context.Context, p Payload) (graphBuildState, error) {
	id := h.newID()
	var state graphBuildState
	err := h.db.QueryRow(ctx, `
		INSERT INTO "CodeGraphIndex" (
			id, provider, status, "sourceRevision", "commitHash", "graphSpace",
			"workspaceId", "schemaVersion", "builderVersion", "indexRunId",
			"vertexCount", "edgeCount", "anchorCount", "linkedEdgeCount",
			"errorMessage", "indexedAt", "orgId", "repoId", "updatedAt"
		) VALUES (
			$1, 'NEBULA', 'BUILDING', $2, $3, $4,
			$5, $6, $7, $8,
			0, 0, 0, 0,
			NULL, NULL, $9, $10, CURRENT_TIMESTAMP
		)
		ON CONFLICT ("repoId", "workspaceId", "commitHash", "provider", "schemaVersion", "builderVersion")
		DO UPDATE SET
			status = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex".status
				ELSE EXCLUDED.status
			END,
			"sourceRevision" = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex"."sourceRevision"
				ELSE EXCLUDED."sourceRevision"
			END,
			"graphSpace" = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex"."graphSpace"
				ELSE EXCLUDED."graphSpace"
			END,
			"indexRunId" = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex"."indexRunId"
				ELSE EXCLUDED."indexRunId"
			END,
			"vertexCount" = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex"."vertexCount"
				ELSE 0
			END,
			"edgeCount" = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex"."edgeCount"
				ELSE 0
			END,
			"anchorCount" = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex"."anchorCount"
				ELSE 0
			END,
			"linkedEdgeCount" = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex"."linkedEdgeCount"
				ELSE 0
			END,
			"indexedAt" = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex"."indexedAt"
				ELSE NULL
			END,
			"errorMessage" = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex"."errorMessage"
				ELSE NULL
			END,
			"updatedAt" = CASE
				WHEN "CodeGraphIndex".status IN ('READY', 'SKIPPED') THEN "CodeGraphIndex"."updatedAt"
				ELSE CURRENT_TIMESTAMP
			END
		RETURNING id, status::text
	`, id, p.Revision, p.CommitHash, graphschema.SpaceName, p.WorkspaceID, int32(p.SchemaVersion),
		p.BuilderVersion, p.IndexJobID, int32(p.OrgID), int32(p.RepoID)).Scan(&state.ID, &state.Status)
	if err != nil {
		return graphBuildState{}, err
	}
	return state, nil
}

func (h *Handler) markReady(ctx context.Context, graphIndexID string, p Payload, result graphschema.CodeGraphWriteResult) error {
	vertexCount, err := countToPostgresInt("vertexCount", result.VertexCount)
	if err != nil {
		return err
	}
	edgeCount, err := countToPostgresInt("edgeCount", result.EdgeCount)
	if err != nil {
		return err
	}
	anchorCount, err := countToPostgresInt("anchorCount", result.AnchorCount)
	if err != nil {
		return err
	}
	linkedEdgeCount, err := countToPostgresInt("linkedEdgeCount", result.LinkedEdgeCount)
	if err != nil {
		return err
	}
	_, err = h.db.Exec(ctx, `
		UPDATE "CodeGraphIndex"
		SET status = 'READY',
		    "graphSpace" = $4,
		    "vertexCount" = $5,
		    "edgeCount" = $6,
		    "anchorCount" = $7,
		    "linkedEdgeCount" = $8,
		    "indexedAt" = CURRENT_TIMESTAMP,
		    "errorMessage" = NULL,
		    "updatedAt" = CURRENT_TIMESTAMP
		WHERE id = $1 AND "orgId" = $2 AND "repoId" = $3
	`, graphIndexID, int32(p.OrgID), int32(p.RepoID), graphschema.SpaceName,
		vertexCount, edgeCount, anchorCount, linkedEdgeCount)
	return err
}

func (h *Handler) repairReadyGraphCounts(ctx context.Context, graphIndexID string, p Payload) error {
	_, err := h.db.Exec(ctx, `
		WITH counts AS (
		  SELECT
		    (
		      SELECT COUNT(*)::int
		      FROM "CodeGraphAnchor" a
		      WHERE a."graphIndexId" = $1
		        AND a."orgId" = $2
		        AND a."repoId" = $3
		    ) AS anchors,
		    (
		      SELECT COUNT(*)::int
		      FROM "CodeGraphSemanticEdge" e
		      WHERE e."graphIndexId" = $1
		        AND e."orgId" = $2
		        AND e."repoId" = $3
		        AND (
		          lower(e.source) = 'anchor-linker'
		          OR EXISTS (
		            SELECT 1
		            FROM "CodeGraphAnchor" a
		            WHERE a."graphIndexId" = e."graphIndexId"
		              AND a."orgId" = e."orgId"
		              AND a."repoId" = e."repoId"
		              AND (a."nodeVid" = e."sourceExternalId" OR a."nodeVid" = e."targetExternalId")
		          )
		        )
		    ) AS linked_edges
		)
		UPDATE "CodeGraphIndex" g
		SET "anchorCount" = counts.anchors,
		    "linkedEdgeCount" = counts.linked_edges,
		    "updatedAt" = CASE
		      WHEN COALESCE(g."anchorCount", 0) IS DISTINCT FROM counts.anchors
		        OR COALESCE(g."linkedEdgeCount", 0) IS DISTINCT FROM counts.linked_edges
		      THEN CURRENT_TIMESTAMP
		      ELSE g."updatedAt"
		    END
		FROM counts
		WHERE g.id = $1
		  AND g."orgId" = $2
		  AND g."repoId" = $3
		  AND g.status = 'READY'::"CodeGraphIndexStatus"
		  AND (
		    COALESCE(g."anchorCount", 0) IS DISTINCT FROM counts.anchors
		    OR COALESCE(g."linkedEdgeCount", 0) IS DISTINCT FROM counts.linked_edges
		  )
	`, graphIndexID, int32(p.OrgID), int32(p.RepoID))
	return err
}

func (h *Handler) markPartial(ctx context.Context, graphIndexID string, p Payload, result graphschema.CodeGraphWriteResult) error {
	vertexCount, err := countToPostgresInt("vertexCount", result.VertexCount)
	if err != nil {
		return err
	}
	edgeCount, err := countToPostgresInt("edgeCount", result.EdgeCount)
	if err != nil {
		return err
	}
	anchorCount, err := countToPostgresInt("anchorCount", result.AnchorCount)
	if err != nil {
		return err
	}
	linkedEdgeCount, err := countToPostgresInt("linkedEdgeCount", result.LinkedEdgeCount)
	if err != nil {
		return err
	}
	_, err = h.db.Exec(ctx, `
		UPDATE "CodeGraphIndex"
		SET status = 'PARTIAL',
		    "graphSpace" = $4,
		    "vertexCount" = $5,
		    "edgeCount" = $6,
		    "anchorCount" = $7,
		    "linkedEdgeCount" = $8,
		    "indexedAt" = NULL,
		    "errorMessage" = NULL,
		    "updatedAt" = CURRENT_TIMESTAMP
		WHERE id = $1 AND "orgId" = $2 AND "repoId" = $3
	`, graphIndexID, int32(p.OrgID), int32(p.RepoID), graphschema.SpaceName,
		vertexCount, edgeCount, anchorCount, linkedEdgeCount)
	return err
}

func (h *Handler) markSkipped(ctx context.Context, graphIndexID string, p Payload, reason string) error {
	_, err := h.db.Exec(ctx, `
		UPDATE "CodeGraphIndex"
		SET status = 'SKIPPED',
		    "vertexCount" = 0,
		    "edgeCount" = 0,
		    "anchorCount" = 0,
		    "linkedEdgeCount" = 0,
		    "indexedAt" = CURRENT_TIMESTAMP,
		    "errorMessage" = $4,
		    "updatedAt" = CURRENT_TIMESTAMP
		WHERE id = $1 AND "orgId" = $2 AND "repoId" = $3
	`, graphIndexID, int32(p.OrgID), int32(p.RepoID), reason)
	return err
}

func (h *Handler) markFailed(ctx context.Context, graphIndexID string, p Payload, reason string) error {
	_, err := h.db.Exec(ctx, `
		UPDATE "CodeGraphIndex"
		SET status = 'FAILED',
		    "vertexCount" = 0,
		    "edgeCount" = 0,
		    "anchorCount" = 0,
		    "linkedEdgeCount" = 0,
		    "indexedAt" = CURRENT_TIMESTAMP,
		    "errorMessage" = $4,
		    "updatedAt" = CURRENT_TIMESTAMP
		WHERE id = $1 AND "orgId" = $2 AND "repoId" = $3
		  AND status NOT IN ('READY', 'SKIPPED')
	`, graphIndexID, int32(p.OrgID), int32(p.RepoID), reason)
	return err
}

func (h *Handler) upsertRevisionIfManifestReady(ctx context.Context, graphIndexID string, p Payload) error {
	tag, err := h.db.Exec(ctx, `
		INSERT INTO "CodeGraphRevision" (
			id, revision, "workspaceId", "commitHash", provider,
			"schemaVersion", "builderVersion", "activatedAt",
			"updatedAt", "orgId", "repoId", "codeGraphIndexId"
		)
		SELECT
			$1, $2, $3, $4, 'NEBULA',
			$5, $6, CURRENT_TIMESTAMP,
			CURRENT_TIMESTAMP, $7, $8, $9
		WHERE EXISTS (
			SELECT 1
			FROM "RepoIndexManifest"
			WHERE "orgId" = $7
			  AND "repoId" = $8
			  AND "workspaceId" = $3
			  AND branch = $13
			  AND "commitHash" = $4
			  AND id = $10
			  AND "indexJobId" = $11
			  AND (("providerConnectionId" IS NULL AND $12::text IS NULL) OR "providerConnectionId" = $12)
			  AND status = 'READY'
		)
		ON CONFLICT ("repoId", "workspaceId", revision, provider, "schemaVersion", "builderVersion")
		DO UPDATE SET
			"commitHash" = EXCLUDED."commitHash",
			"codeGraphIndexId" = EXCLUDED."codeGraphIndexId",
			"activatedAt" = CURRENT_TIMESTAMP,
			"updatedAt" = CURRENT_TIMESTAMP
	`, h.newID(), p.Revision, p.WorkspaceID, p.CommitHash, int32(p.SchemaVersion),
		p.BuilderVersion, int32(p.OrgID), int32(p.RepoID), graphIndexID,
		p.ManifestID, p.IndexJobID, p.providerConnectionArg(), p.manifestBranch())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errStaleManifest
	}
	return nil
}

func (p Payload) providerConnectionArg() any {
	if p.ProviderConnectionID == nil {
		return nil
	}
	return *p.ProviderConnectionID
}

func skipRetry(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, asynq.SkipRetry)...)
}

func isHexCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !unicode.Is(unicode.ASCII_Hex_Digit, r) {
			return false
		}
	}
	return true
}

func shortCommit(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

func countToPostgresInt(name string, value int64) (int32, error) {
	if value < 0 || value > maxPostgresInt {
		return 0, fmt.Errorf("%s=%d outside Postgres INTEGER range", name, value)
	}
	return int32(value), nil
}
