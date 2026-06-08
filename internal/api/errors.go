package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ServiceError is the JSON envelope every error response uses.
// Field ordering (statusCode, errorCode, message) is enforced by
// the declaration order plus the json struct tags; encoding/json
// preserves declaration order in the emitted JSON.
type ServiceError struct {
	StatusCode int    `json:"statusCode"`
	ErrorCode  string `json:"errorCode"`
	Message    string `json:"message"`
}

// errorCode constants used by handler glue. New error surfaces
// extend the set as needed.
const (
	errorCodeNotAuthenticated       = "NOT_AUTHENTICATED"
	errorCodeInsufficientPermission = "INSUFFICIENT_PERMISSIONS"
	errorCodeUnexpectedError        = "UNEXPECTED_ERROR"
	errorCodeInvalidRequestBody     = "INVALID_REQUEST_BODY"
	errorCodeInvalidQueryParams     = "INVALID_QUERY_PARAMS"
)

// Pre-marshalled response bodies for the common error cases.
// Caching the byte slice (a) saves a per-request encoding/json
// allocation, and (b) locks the byte-equal payload — a future
// change to the shape would have to update the literal here,
// making drift obvious.
var (
	notAuthenticatedBody = mustMarshal(ServiceError{
		StatusCode: http.StatusUnauthorized,
		ErrorCode:  errorCodeNotAuthenticated,
		Message:    "Not authenticated",
	})
	unexpectedErrorBody = mustMarshal(ServiceError{
		StatusCode: http.StatusInternalServerError,
		ErrorCode:  errorCodeUnexpectedError,
		Message:    "An unexpected error occurred",
	})
)

// mustMarshal is a startup-only helper: any failure here is a
// programmer error, not a runtime one. Pre-marshalling at init
// avoids per-request allocation and locks the byte-equal payload.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("api: failed to marshal static error body: " + err.Error())
	}
	return b
}

// writeServiceError renders a ServiceError as a JSON response with
// the matching HTTP status. Allocates once for the marshalled body.
//
// A json.Marshal failure here is impossible for the ServiceError
// type (three primitive fields) but guarded defensively. The
// fallback path emits a generic 500 payload AND a slog line so the
// impossible case is observable in production logs.
func writeServiceError(w http.ResponseWriter, e ServiceError, logger *slog.Logger) {
	body, err := json.Marshal(e)
	if err != nil {
		if logger != nil {
			logger.Error("writeServiceError: failed to marshal ServiceError", "err", err, "statusCode", e.StatusCode, "errorCode", e.ErrorCode)
		}
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(body)
}

// writeStaticServiceError writes a pre-marshalled response body
// with the supplied status. Used for the no-allocation hot-path
// error responses.
func writeStaticServiceError(w http.ResponseWriter, statusCode int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}
