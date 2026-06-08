package auth

import (
	"errors"
	"strings"
)

// ApiKeyPrefix is the canonical prefix every API key carries on the
// wire. Issued tokens take the form `<ApiKeyPrefix><64-hex-secret>`.
const ApiKeyPrefix = "cik_"

// ErrInvalidApiKey is returned when the raw key string is malformed:
// missing the canonical prefix, or carrying the prefix with an
// empty secret tail. An empty input is treated as invalid.
//
// The middleware converts this into a 401 response. It deliberately
// does not log the offending input — keys are credential material.
var ErrInvalidApiKey = errors.New("auth: api key is malformed")

// ParseApiKey strips ApiKeyPrefix from raw and returns the secret
// payload that callers feed into HashSecret. Anything else returns
// ErrInvalidApiKey.
//
// Splitting prefix-strip from the hash + lookup steps lets the hash
// computation be tested without a database and keeps the SQL queries
// free of input-validation branches.
func ParseApiKey(raw string) (string, error) {
	if raw == "" {
		return "", ErrInvalidApiKey
	}
	secret, ok := strings.CutPrefix(raw, ApiKeyPrefix)
	if !ok || secret == "" {
		return "", ErrInvalidApiKey
	}
	return secret, nil
}
