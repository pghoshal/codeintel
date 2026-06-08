// GET /api/connections/{id}/branches — expose the branch-sync
// policy derived from the connection's config.revisions.branches
// field.
//
// Policy semantics:
//   - mode="default"  — no `branches` setting; only the repo's
//     default branch is indexed.
//   - mode="all"      — the literal `["*"]` value means index
//     every branch the host returns.
//   - mode="patterns" — an explicit list (deduped, whitespace-
//     trimmed); each entry is a branch name or glob pattern.
//
// maxIndexedRevisions is the deployment-wide ceiling on how many
// revisions a single connection can index; pinned at 64 to keep
// indexer fan-out bounded.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// allBranchesGlob is the sentinel value that puts a connection in
// "index every branch" mode. Wire-format frozen.
const allBranchesGlob = "*"

// maxIndexedRevisions caps how many revisions a single connection
// can index. Tunable in a future config slice; pinned today.
const maxIndexedRevisions = 64

// branchSyncMode is the typed enum the response carries. String-
// typed so JSON serialisation needs no MarshalJSON override.
type branchSyncMode string

const (
	branchSyncModeDefault  branchSyncMode = "default"
	branchSyncModeAll      branchSyncMode = "all"
	branchSyncModePatterns branchSyncMode = "patterns"
)

// branchPolicy is the nested object the response embeds. Field
// order is pinned by struct declaration order; encoding/json
// preserves it.
type branchPolicy struct {
	Mode                       branchSyncMode `json:"mode"`
	Branches                   []string       `json:"branches"`
	DefaultBranchAlwaysIncluded bool          `json:"defaultBranchAlwaysIncluded"`
	MaxIndexedRevisions         int           `json:"maxIndexedRevisions"`
}

// branchesResponse is the wire shape for the GET. JSON field
// order matches the response example in the slice doc-comment.
type branchesResponse struct {
	ConnectionID   int32           `json:"connectionId"`
	ConnectionName string          `json:"connectionName"`
	ConnectionType string          `json:"connectionType"`
	SyncedAt       *iso8601MilliTime `json:"syncedAt"`
	UpdatedAt      iso8601MilliTime  `json:"updatedAt"`
	BranchPolicy   branchPolicy    `json:"branchPolicy"`
}

// normaliseBranches dedupes, trims, and filters empty entries from
// a raw branches list. Pure function so tests can lock the
// algorithm in isolation if needed.
func normaliseBranches(raw []string) []string {
	if len(raw) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, b := range raw {
		t := strings.TrimSpace(b)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// computeBranchPolicy derives the policy object from a decoded
// connection config. The walker is defensive: missing /
// non-object /  non-string-array shapes yield the "default" mode
// rather than 500.
func computeBranchPolicy(config any) branchPolicy {
	branches := branchesFromConfig(config)
	mode := branchSyncModeDefault
	for _, b := range branches {
		if b == allBranchesGlob {
			mode = branchSyncModeAll
			break
		}
	}
	if len(branches) > 0 && mode != branchSyncModeAll {
		mode = branchSyncModePatterns
	}
	return branchPolicy{
		Mode:                       mode,
		Branches:                   branches,
		DefaultBranchAlwaysIncluded: mode == branchSyncModeDefault,
		MaxIndexedRevisions:         maxIndexedRevisions,
	}
}

// branchesFromConfig safely extracts the config.revisions.branches
// string array, returning an empty slice on any shape mismatch.
func branchesFromConfig(config any) []string {
	cfg, ok := config.(map[string]any)
	if !ok {
		return []string{}
	}
	rev, ok := cfg["revisions"].(map[string]any)
	if !ok {
		return []string{}
	}
	rawBranches, ok := rev["branches"].([]any)
	if !ok {
		return []string{}
	}
	raw := make([]string, 0, len(rawBranches))
	for _, b := range rawBranches {
		if s, ok := b.(string); ok {
			raw = append(raw, s)
		}
	}
	return normaliseBranches(raw)
}

func (s *Server) handleGetOrgConnectionBranches(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.connectionsLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if authCtx.Role != auth.OrgRoleOwner {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusForbidden,
			ErrorCode:  errorCodeInsufficientPermission,
			Message:    "Only organization owners can view branch sync policy.",
		}, s.connectionsLogger)
		return
	}

	idStr := r.PathValue("id")
	idParsed, parseErr := strconv.ParseInt(idStr, 10, 32)
	if parseErr != nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidQueryParams,
			Message:    "Connection id must be an integer.",
		}, s.connectionsLogger)
		return
	}
	connectionID := int32(idParsed)

	row, err := s.cfg.Queries.GetOrgConnectionMeta(r.Context(), authCtx.Org.ID, connectionID)
	if err != nil {
		if errors.Is(err, db.ErrConnectionNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Connection not found.",
			}, s.connectionsLogger)
			return
		}
		s.connectionsLogger.Error("GetOrgConnectionMeta failed (branches)", "err", err, "orgId", authCtx.Org.ID, "connectionId", connectionID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	resp := branchesResponse{
		ConnectionID:   row.ID,
		ConnectionName: row.Name,
		ConnectionType: row.ConnectionType,
		SyncedAt:       syncedAtFromRow(row.SyncedAt),
		UpdatedAt:      iso8601MilliTime(row.UpdatedAt),
		BranchPolicy:   computeBranchPolicy(row.Config),
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.connectionsLogger.Error("encode branches response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
