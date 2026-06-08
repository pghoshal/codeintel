package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// maxPutOrgSecretBodyBytes caps the request body size for
// PUT /api/secrets at 1 MiB. The handler decodes the body into a
// putOrgSecretBody struct; without this cap a malicious client
// could stream an unbounded `value` field and exhaust process
// memory. Enforced explicitly at the route boundary.
const maxPutOrgSecretBodyBytes = 1 << 20

// orgSecretResponse is the per-row response shape for the
// /api/secrets GET and PUT handlers:
//
//	{
//	  "key": "<key>",
//	  "createdAt": "<iso>",
//	  "updatedAt": "<iso>",
//	  "ref": { "secretRef": "<key>" }
//	}
//
// Field ordering matches struct declaration order, which is the
// order encoding/json emits.
type orgSecretResponse struct {
	Key       string         `json:"key"`
	CreatedAt iso8601MilliTime `json:"createdAt"`
	UpdatedAt iso8601MilliTime `json:"updatedAt"`
	Ref       secretRef      `json:"ref"`
}

// secretRef is the nested object the response wraps each row's key
// into so callers see the canonical secret-reference shape that
// connection / model config blobs also carry.
type secretRef struct {
	Key string `json:"secretRef"`
}

// iso8601MilliTime wraps time.Time to emit the ISO-8601 format
// `YYYY-MM-DDTHH:MM:SS.sssZ` (three-digit fixed-precision
// milliseconds, always UTC). Go's default time.Time.MarshalJSON
// uses RFC3339Nano which varies digit count and can emit
// timezone-offset suffixes — both are wrong for this wire shape.
type iso8601MilliTime time.Time

// MarshalJSON emits the UTC ISO-8601 representation. Examples:
//
//	2025-01-15T10:30:00.000Z   (exactly midnight-of-second)
//	2025-02-20T08:15:00.500Z   (half-second past)
//
// Always 3-digit milliseconds, always 'Z' suffix.
func (p iso8601MilliTime) MarshalJSON() ([]byte, error) {
	t := time.Time(p).UTC()
	formatted := t.Format(`"2006-01-02T15:04:05.000Z"`)
	return []byte(formatted), nil
}

// handleListOrgSecrets handles GET /api/secrets:
//
//  1. Resolve API-key auth.
//  2. OWNER role guard.
//  3. ListOrgSecrets — fetch the per-org rows.
//  4. Map to the response shape and encode.
func (s *Server) handleListOrgSecrets(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		// Every recognised auth failure collapses to a uniform 401
		// — no information leak about which check failed.
		//
		// NOTE: there is a known timing-oracle surface here:
		// ErrNoCredentials short-circuits before any DB call (fast),
		// ErrUnknownKey hits the JOIN (slower). The 401 body is
		// byte-identical but response time leaks "key shape was
		// valid". A constant-time-delay shim is queued for the
		// security-hardening track.
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		// Any other error (DB outage, lastUsedAt UPDATE failure,
		// etc.) is a 500. Raw error never leaks to the client; the
		// log captures it.
		s.secretsLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	if authCtx.Role != auth.OrgRoleOwner {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusForbidden,
			ErrorCode:  errorCodeInsufficientPermission,
			Message:    "Only organization owners can list secrets.",
		}, s.secretsLogger)
		return
	}

	rows, err := s.cfg.Queries.ListOrgSecrets(r.Context(), authCtx.Org.ID)
	if err != nil {
		s.secretsLogger.Error("ListOrgSecrets failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	body := buildOrgSecretsResponse(rows)
	encoded, err := json.Marshal(body)
	if err != nil {
		s.secretsLogger.Error("encode secrets response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// isAuthFailure reports whether err is one of the auth-layer
// sentinels that collapse to a 401 response. Centralising the check
// avoids a four-call errors.Is chain on every failed-auth request
// (each errors.Is unwraps the cause chain).
func isAuthFailure(err error) bool {
	switch {
	case errors.Is(err, auth.ErrNoCredentials),
		errors.Is(err, auth.ErrMalformedKey),
		errors.Is(err, auth.ErrUnknownKey),
		errors.Is(err, auth.ErrGuestRole):
		return true
	}
	return false
}

// secretKeyRegex constrains the secret key character set:
// min(1).max(128).regex(`^[A-Za-z0-9_.:\-]+$`). Compiled once at
// init so the per-request branch is a hot-path lookup.
var secretKeyRegex = regexp.MustCompile(`^[A-Za-z0-9_.:\-]+$`)

// putOrgSecretBody is the request body schema for PUT /api/secrets:
//
//	{ "key": "<id>", "value": "<plaintext>" }
//
// Each field is required; absence (or wrong type) surfaces as a
// JSON decode failure and the handler returns 400.
type putOrgSecretBody struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// validate returns a non-nil ServiceError if the body fails schema
// constraints. The handler treats all schema failures as 400.
func (b *putOrgSecretBody) validate() (string, bool) {
	if b.Key == "" {
		return "Secret key is required.", false
	}
	if l := len(b.Key); l > 128 {
		return "Secret key must be at most 128 characters.", false
	}
	if !secretKeyRegex.MatchString(b.Key) {
		return "Secret key contains disallowed characters.", false
	}
	if b.Value == "" {
		return "Secret value is required.", false
	}
	return "", true
}

// handlePutOrgSecret handles PUT /api/secrets:
//
//  1. Resolve API-key auth.
//  2. OWNER role guard.
//  3. JSON decode + schema validation.
//  4. Encrypt the value with the configured encryption key.
//  5. UpsertOrgSecret in one statement; receive the row back.
//  6. Encode the response (same shape as GET emits for a single row).
func (s *Server) handlePutOrgSecret(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.secretsLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	if authCtx.Role != auth.OrgRoleOwner {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusForbidden,
			ErrorCode:  errorCodeInsufficientPermission,
			Message:    "Only organization owners can create or update secrets.",
		}, s.secretsLogger)
		return
	}

	// Bound the request body so a malicious client cannot exhaust
	// memory by streaming a giant `value`. MaxBytesReader trips the
	// subsequent Decode with a recognisable error.
	r.Body = http.MaxBytesReader(w, r.Body, maxPutOrgSecretBodyBytes)

	var body putOrgSecretBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusRequestEntityTooLarge,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body exceeds the maximum allowed size.",
			}, s.secretsLogger)
			return
		}
		// Distinguish io.EOF (empty body) from malformed JSON for a
		// more useful diagnostic message.
		if errors.Is(err, io.EOF) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is empty.",
			}, s.secretsLogger)
			return
		}
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Request body is not valid JSON.",
		}, s.secretsLogger)
		return
	}
	if msg, ok := body.validate(); !ok {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    msg,
		}, s.secretsLogger)
		return
	}

	iv, ciphertext, err := auth.Encrypt(s.cfg.EncryptionKey, body.Value)
	if err != nil {
		s.secretsLogger.Error("encrypt secret failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	row, err := s.cfg.Queries.UpsertOrgSecret(r.Context(), db.UpsertOrgSecretParams{
		OrgID:          authCtx.Org.ID,
		Key:            body.Key,
		EncryptedValue: ciphertext,
		IV:             iv,
	})
	if err != nil {
		s.secretsLogger.Error("UpsertOrgSecret failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	resp := orgSecretResponse{
		Key:       row.Key,
		CreatedAt: iso8601MilliTime(row.CreatedAt),
		UpdatedAt: iso8601MilliTime(row.UpdatedAt),
		Ref:       secretRef{Key: row.Key},
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.secretsLogger.Error("encode put response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// buildOrgSecretsResponse maps db.OrgSecret rows to the wire-format
// shape. Returning `make([]..., 0)` (never nil) ensures an empty
// result encodes as `[]`, not `null`.
func buildOrgSecretsResponse(rows []db.OrgSecret) []orgSecretResponse {
	out := make([]orgSecretResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, orgSecretResponse{
			Key:       r.Key,
			CreatedAt: iso8601MilliTime(r.CreatedAt),
			UpdatedAt: iso8601MilliTime(r.UpdatedAt),
			Ref:       secretRef{Key: r.Key},
		})
	}
	return out
}
