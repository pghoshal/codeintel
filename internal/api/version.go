package api

import (
	"encoding/json"
	"net/http"
	"os"
)

// defaultCodeintelVersion is the build-time fallback when the
// CODEINTEL_VERSION env var is unset. Static binaries can override
// this via -ldflags at build time; container images set
// CODEINTEL_VERSION at run time. The fallback keeps non-containerised
// runs emitting a non-empty version string.
//
// Operator constraint: CODEINTEL_VERSION is a build-identity string
// only. It must never carry secrets or attacker-controllable data;
// the value flows into the public /api/version response body. JSON
// metacharacters are safely escaped by encoding/json so a hostile
// value cannot break the envelope, but pinning the env var to a
// controlled `^[A-Za-z0-9._+\-]+$`-style string at deploy time is
// recommended.
var defaultCodeintelVersion = "v0.1.0-dev"

// versionResponse is the wire shape `{"version":"<value>"}`. Using
// a typed struct + json.Marshal escapes any metacharacters in the
// version string, defending against an attacker who can poison the
// build pipeline from injecting top-level keys via this field.
type versionResponse struct {
	Version string `json:"version"`
}

// resolveVersion picks the env-var override over the build-time
// default. Both sources are read at request time so a container env
// update is picked up without restarting the process.
func resolveVersion() string {
	if v := os.Getenv("CODEINTEL_VERSION"); v != "" {
		return v
	}
	return defaultCodeintelVersion
}

// versionResponseBytes produces the wire-format payload. json.Marshal
// (unlike json.Encoder.Encode) does not append a trailing newline,
// so the output stays compact and stable.
func versionResponseBytes(version string) ([]byte, error) {
	return json.Marshal(versionResponse{Version: version})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	body, err := versionResponseBytes(resolveVersion())
	if err != nil {
		// Marshal cannot fail for a struct containing only a string
		// field — defensive only; never observed in practice.
		http.Error(w, `{"error":"version marshal failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
