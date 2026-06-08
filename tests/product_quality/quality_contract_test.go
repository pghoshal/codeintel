package productquality

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

type qualitySpec struct {
	Version           int                 `json:"version"`
	Scenario          string              `json:"scenario"`
	MinimumRepoCount  int                 `json:"minimumRepoCount"`
	Layers            []string            `json:"layers"`
	LatencyBudgets    latencyBudgets      `json:"latencyBudgets"`
	QualityThresholds qualityThresholds   `json:"qualityThresholds"`
	Repositories      []qualityRepository `json:"repositories"`
	ReferenceProjects []referenceProject  `json:"referenceProjects"`
	Queries           []qualityQuery      `json:"queries"`
}

type latencyBudgets struct {
	SearchP50Ms       int `json:"searchP50Ms"`
	SearchP99Ms       int `json:"searchP99Ms"`
	SymbolP50Ms       int `json:"symbolP50Ms"`
	SymbolP99Ms       int `json:"symbolP99Ms"`
	GraphInspectP50Ms int `json:"graphInspectP50Ms"`
	GraphInspectP99Ms int `json:"graphInspectP99Ms"`
	AskCodebaseP50Ms  int `json:"askCodebaseP50Ms"`
	AskCodebaseP99Ms  int `json:"askCodebaseP99Ms"`
}

type qualityThresholds struct {
	RequiredEvidenceRecall    float64 `json:"requiredEvidenceRecall"`
	FalsePositiveRateMax      float64 `json:"falsePositiveRateMax"`
	TenantLeakageMax          int     `json:"tenantLeakageMax"`
	GraphValueDeltaMin        float64 `json:"graphValueDeltaMin"`
	AnswerCompletenessMin     float64 `json:"answerCompletenessMin"`
	SourceCitationCoverageMin float64 `json:"sourceCitationCoverageMin"`
}

type qualityRepository struct {
	Name          string   `json:"name"`
	URL           string   `json:"url"`
	DefaultBranch string   `json:"defaultBranch"`
	Languages     []string `json:"languages"`
	Anchors       []string `json:"anchors"`
}

type referenceProject struct {
	Name          string   `json:"name"`
	RequiredIdeas []string `json:"requiredIdeas"`
}

type qualityQuery struct {
	ID                     string   `json:"id"`
	Question               string   `json:"question"`
	RequiredLayers         []string `json:"requiredLayers"`
	RequiredAnswerSections []string `json:"requiredAnswerSections"`
}

func loadQualitySpec(t *testing.T) qualitySpec {
	t.Helper()
	body, err := os.ReadFile("enterprise_quality_spec.json")
	if err != nil {
		t.Fatalf("read enterprise_quality_spec.json: %v", err)
	}
	var spec qualitySpec
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("decode enterprise_quality_spec.json: %v", err)
	}
	return spec
}

func TestEnterpriseQualitySpecCoversTenRelatedPolyglotRepos(t *testing.T) {
	spec := loadQualitySpec(t)
	if spec.Version != 1 {
		t.Fatalf("version: got %d, want 1", spec.Version)
	}
	if len(spec.Repositories) < spec.MinimumRepoCount || len(spec.Repositories) < 10 {
		t.Fatalf("repo count: got %d, want at least %d and never below 10", len(spec.Repositories), spec.MinimumRepoCount)
	}

	names := make(map[string]struct{}, len(spec.Repositories))
	languages := map[string]bool{}
	sharedAnchors := map[string]int{}
	for _, repo := range spec.Repositories {
		if repo.Name == "" || repo.URL == "" || repo.DefaultBranch == "" {
			t.Fatalf("repo has incomplete identity: %+v", repo)
		}
		if _, exists := names[repo.Name]; exists {
			t.Fatalf("duplicate repo name %q", repo.Name)
		}
		names[repo.Name] = struct{}{}
		if len(repo.Languages) == 0 {
			t.Fatalf("repo %s has no languages", repo.Name)
		}
		if len(repo.Anchors) == 0 {
			t.Fatalf("repo %s has no cross-repo anchors", repo.Name)
		}
		for _, language := range repo.Languages {
			languages[language] = true
		}
		for _, anchor := range repo.Anchors {
			sharedAnchors[anchor]++
		}
	}

	for _, want := range []string{"go", "typescript", "python", "java", "dotnet", "rust"} {
		if !languages[want] {
			t.Fatalf("missing required language family %q in real-repo scenario", want)
		}
	}
	for _, anchor := range []string{"otlp", "trace", "metric", "collector"} {
		if sharedAnchors[anchor] < 2 {
			t.Fatalf("anchor %q appears in %d repo(s), want at least 2 for cross-repo linking", anchor, sharedAnchors[anchor])
		}
	}
}

func TestEnterpriseQualitySpecRequiresFullLayeredRetrieval(t *testing.T) {
	spec := loadQualitySpec(t)
	for _, layer := range []string{"zoekt", "scip", "ast", "tree-sitter", "graph", "mcp"} {
		if !slices.Contains(spec.Layers, layer) {
			t.Fatalf("missing retrieval/indexing layer %q", layer)
		}
	}
	if len(spec.Queries) < 4 {
		t.Fatalf("query count: got %d, want at least 4 enterprise architecture/code-flow scenarios", len(spec.Queries))
	}
	for _, query := range spec.Queries {
		if query.ID == "" || query.Question == "" {
			t.Fatalf("query missing identity/text: %+v", query)
		}
		if !slices.Contains(query.RequiredLayers, "mcp") {
			t.Fatalf("query %s must validate MCP output, layers=%v", query.ID, query.RequiredLayers)
		}
		for _, section := range []string{"exact-files"} {
			if !slices.Contains(query.RequiredAnswerSections, section) {
				t.Fatalf("query %s must require %q evidence", query.ID, section)
			}
		}
	}
}

func TestEnterpriseQualitySpecHasStrictQualityAndLatencyBudgets(t *testing.T) {
	spec := loadQualitySpec(t)
	if spec.QualityThresholds.RequiredEvidenceRecall < 0.9 {
		t.Fatalf("required evidence recall %.2f is below enterprise bar", spec.QualityThresholds.RequiredEvidenceRecall)
	}
	if spec.QualityThresholds.FalsePositiveRateMax > 0.08 {
		t.Fatalf("false positive ceiling %.2f is too loose", spec.QualityThresholds.FalsePositiveRateMax)
	}
	if spec.QualityThresholds.TenantLeakageMax != 0 {
		t.Fatalf("tenant leakage max must be zero, got %d", spec.QualityThresholds.TenantLeakageMax)
	}
	if spec.QualityThresholds.GraphValueDeltaMin < 0.2 {
		t.Fatalf("graph value delta %.2f is too low to prove graph adds value", spec.QualityThresholds.GraphValueDeltaMin)
	}
	if spec.QualityThresholds.SourceCitationCoverageMin < 0.95 {
		t.Fatalf("source citation coverage %.2f is below enterprise bar", spec.QualityThresholds.SourceCitationCoverageMin)
	}

	b := spec.LatencyBudgets
	checkBudget := func(name string, p50, p99 int) {
		t.Helper()
		if p50 <= 0 || p99 <= 0 {
			t.Fatalf("%s latency budget must be positive: p50=%d p99=%d", name, p50, p99)
		}
		if p99 > p50*4 {
			t.Fatalf("%s p99 budget must stay within 4x p50: p50=%d p99=%d", name, p50, p99)
		}
	}
	checkBudget("search", b.SearchP50Ms, b.SearchP99Ms)
	checkBudget("symbol", b.SymbolP50Ms, b.SymbolP99Ms)
	checkBudget("graphInspect", b.GraphInspectP50Ms, b.GraphInspectP99Ms)
	checkBudget("askCodebase", b.AskCodebaseP50Ms, b.AskCodebaseP99Ms)
}

func TestReferenceProjectMaturityCoverageIsExplicit(t *testing.T) {
	spec := loadQualitySpec(t)
	requiredRefs := []string{
		"Jakedismo/codegraph-rust",
		"safishamsi/graphify",
		"colbymchenry/codegraph",
		"FalkorDB/code-graph",
		"getzep/graphiti",
		"tirth8205/code-review-graph",
	}
	seen := map[string]referenceProject{}
	for _, ref := range spec.ReferenceProjects {
		seen[ref.Name] = ref
	}
	for _, name := range requiredRefs {
		ref, ok := seen[name]
		if !ok {
			t.Fatalf("missing reference project %s", name)
		}
		if len(ref.RequiredIdeas) < 3 {
			t.Fatalf("reference project %s has only %d required ideas; maturity drift would be invisible", name, len(ref.RequiredIdeas))
		}
	}
}
