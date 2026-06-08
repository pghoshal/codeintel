// Product-flow parity contract tests.
//
// These tests do not claim full legacy-vs-codeintel parity yet. They lock
// the harness surface so future slices cannot quietly fall back to the
// old three-endpoint static smoke. The live runner in run.sh captures
// request/response evidence against a real deployment when
// CODEINTEL_PARITY_BASE_URL is supplied.
package parity

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

type flowStep struct {
	id     string
	method string
	path   string
}

var requiredFlowSteps = []flowStep{
	{id: "health", method: "GET", path: "/api/health"},
	{id: "version", method: "GET", path: "/api/version"},
	{id: "list_secrets", method: "GET", path: "/api/secrets"},
	{id: "put_secret", method: "PUT", path: "/api/secrets"},
	{id: "put_models", method: "PUT", path: "/api/models"},
	{id: "list_models", method: "GET", path: "/api/models"},
	{id: "post_connection", method: "POST", path: "/api/connections"},
	{id: "list_connections", method: "GET", path: "/api/connections"},
	{id: "put_connection_branches", method: "PUT", path: "/api/connections/{id}/branches"},
	{id: "sync_connection", method: "POST", path: "/api/connections/{id}/sync"},
	{id: "list_repos", method: "GET", path: "/api/repos"},
	{id: "post_repo_index", method: "POST", path: "/api/repos/{id}/index"},
	{id: "delete_repo_index", method: "DELETE", path: "/api/repos/{id}/index"},
	{id: "status", method: "GET", path: "/api/status"},
}

func TestProductFlowContractCoversRequiredLifecycle(t *testing.T) {
	body, err := os.ReadFile("tolerances.yaml")
	if err != nil {
		t.Fatalf("read tolerances.yaml: %v", err)
	}
	text := string(body)
	for _, step := range requiredFlowSteps {
		for _, want := range []string{
			"id: " + step.id,
			"method: " + step.method,
			"path: " + step.path,
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("tolerances.yaml missing %q for step %s", want, step.id)
			}
		}
	}
	if !strings.Contains(text, "mode: strict-by-default") {
		t.Fatalf("tolerances.yaml must stay strict-by-default")
	}
}

func TestLiveCaptureScriptSyntaxAndStepCoverage(t *testing.T) {
	cmd := exec.Command("bash", "-n", "run.sh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n run.sh failed: %v\n%s", err, string(out))
	}

	body, err := os.ReadFile("run.sh")
	if err != nil {
		t.Fatalf("read run.sh: %v", err)
	}
	text := string(body)
	for _, step := range requiredFlowSteps {
		if !strings.Contains(text, step.id) {
			t.Fatalf("run.sh does not capture required step %q", step.id)
		}
	}
	for _, requiredEnv := range []string{
		"CODEINTEL_PARITY_BASE_URL",
		"CODEINTEL_PARITY_API_KEY",
		"CODEINTEL_PARITY_MUTATE",
		"CODEINTEL_PARITY_CONNECTION_ID",
		"CODEINTEL_PARITY_REPO_ID",
	} {
		if !strings.Contains(text, requiredEnv) {
			t.Fatalf("run.sh missing environment knob %s", requiredEnv)
		}
	}
}
