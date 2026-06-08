//go:build realrepo

package productquality

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLiveProductLifecycleArtifactRequired(t *testing.T) {
	reportPath := strings.TrimSpace(os.Getenv("CODEINTEL_PRODUCT_LIFECYCLE_REPORT"))
	if reportPath == "" {
		t.Fatalf("set CODEINTEL_PRODUCT_LIFECYCLE_REPORT to the lifecycle report produced by the product gate")
	}
	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read lifecycle report %s: %v", reportPath, err)
	}
	text := string(body)
	required := []string{
		"Atom creates two org tenants",
		"Repository sync and index lifecycle",
		"remove",
		"MCP",
		"ask_codebase",
		"Layered answer comparison artifacts",
		"Zoekt-only response artifact",
		"Zoekt+SCIP definitions response artifact",
		"Zoekt+SCIP+AST/tree-sitter+graph response artifact",
		"10-round contextual chat continuity",
		"query-only follow-up",
	}
	for _, needle := range required {
		if !strings.Contains(text, needle) {
			t.Fatalf("lifecycle report %s does not contain required proof %q", reportPath, needle)
		}
	}
}

func TestRealRepoURLsReachable(t *testing.T) {
	spec := loadQualitySpec(t)
	for _, repo := range spec.Repositories {
		repo := repo
		t.Run(repo.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ref := "refs/heads/" + repo.DefaultBranch
			cmd := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", "--heads", repo.URL, ref)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("git ls-remote timed out for %s %s", repo.URL, ref)
			}
			if err != nil {
				t.Fatalf("git ls-remote failed for %s %s: %v\n%s", repo.URL, ref, err, string(out))
			}
			if !bytes.Contains(out, []byte(ref)) {
				t.Fatalf("git ls-remote output for %s did not include %s:\n%s", repo.URL, ref, string(out))
			}
		})
	}
}

func TestLiveProductEndpointsRequiredForQualityGate(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("CODEINTEL_PRODUCT_BASE_URL"), "/")
	apiKey := os.Getenv("CODEINTEL_PRODUCT_API_KEY")
	orgDomain := os.Getenv("CODEINTEL_PRODUCT_ORG_DOMAIN")
	if baseURL == "" || apiKey == "" || orgDomain == "" {
		t.Fatalf("set CODEINTEL_PRODUCT_BASE_URL, CODEINTEL_PRODUCT_API_KEY, and CODEINTEL_PRODUCT_ORG_DOMAIN to run the live product quality gate")
	}
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		t.Fatalf("CODEINTEL_PRODUCT_BASE_URL is invalid: %v", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	endpoints := []struct {
		name       string
		method     string
		path       string
		needAPIKey bool
	}{
		{name: "health", method: http.MethodGet, path: "/api/health"},
		{name: "version", method: http.MethodGet, path: "/api/version"},
		{name: "status", method: http.MethodGet, path: "/api/status", needAPIKey: true},
		{name: "search", method: http.MethodPost, path: "/api/search", needAPIKey: true},
		{name: "mcp", method: http.MethodPost, path: "/api/" + orgDomain + "/mcp", needAPIKey: true},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			req, err := http.NewRequest(endpoint.method, baseURL+endpoint.path, strings.NewReader(`{}`))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Accept", "application/json")
			if endpoint.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			if endpoint.needAPIKey {
				req.Header.Set("X-Api-Key", apiKey)
			}
			start := time.Now()
			res, err := client.Do(req)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("%s %s failed after %s: %v", endpoint.method, endpoint.path, elapsed, err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
			if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
				t.Fatalf("%s %s missing or wrong method: status=%d body=%q", endpoint.method, endpoint.path, res.StatusCode, string(body))
			}
			if res.StatusCode >= 500 {
				t.Fatalf("%s %s returned server error: status=%d body=%q", endpoint.method, endpoint.path, res.StatusCode, string(body))
			}
		})
	}
}
