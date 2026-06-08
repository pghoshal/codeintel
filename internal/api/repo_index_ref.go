package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type repoIndexScopeRequest struct {
	Ref    string `json:"ref"`
	Branch string `json:"branch"`
}

func parseRepoIndexRef(r *http.Request) string {
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = strings.TrimSpace(r.URL.Query().Get("branch"))
	}
	if ref != "" || r.Body == nil || r.Body == http.NoBody {
		return ref
	}
	defer r.Body.Close()
	var body repoIndexScopeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		return ref
	}
	if strings.TrimSpace(body.Ref) != "" {
		return strings.TrimSpace(body.Ref)
	}
	return strings.TrimSpace(body.Branch)
}
