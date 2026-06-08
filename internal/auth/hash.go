// Package auth resolves per-request authentication state. It hashes
// API-key secrets, parses prefixed key strings into their secret
// payload, and turns the resulting hash into a typed AuthContext
// the HTTP handlers consume.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// HashSecret returns the lowercase-hex HMAC-SHA256 digest of message
// keyed with the supplied key. The encryption key argument is read
// from CODEINTEL_ENCRYPTION_KEY at startup; the same key is used
// across all hash sites so a single configuration value controls
// the entire credential identity space.
//
// The 64-character lowercase output is suitable for direct equality
// comparison against ApiKey.hash column values without any
// case-folding or re-encoding at the SQL boundary.
func HashSecret(key, message string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}
