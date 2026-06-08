package workerclasses

import (
	"os"
	"sort"
	"testing"

	"codeintel/pkg/asynqueues"

	"go.yaml.in/yaml/v2"
)

func TestAllHasStableUniqueClasses(t *testing.T) {
	classes := All()
	if len(classes) != 8 {
		t.Fatalf("class count: got %d want 8", len(classes))
	}
	seenNames := map[string]struct{}{}
	seenQueues := map[string]struct{}{}
	for _, c := range classes {
		if c.Name == "" {
			t.Fatal("class name must not be empty")
		}
		if c.QueueName == "" {
			t.Fatalf("%s queue name must not be empty", c.Name)
		}
		if _, ok := seenNames[c.Name]; ok {
			t.Fatalf("duplicate class name %q", c.Name)
		}
		if _, ok := seenQueues[c.QueueName]; ok {
			t.Fatalf("duplicate queue name %q", c.QueueName)
		}
		seenNames[c.Name] = struct{}{}
		seenQueues[c.QueueName] = struct{}{}
		if c.PodConcurrency <= 0 {
			t.Fatalf("%s pod concurrency must be positive", c.Name)
		}
		if c.MaxPods <= 0 {
			t.Fatalf("%s max pods must be positive", c.Name)
		}
	}
}

func TestHotAndColdClasses(t *testing.T) {
	hot := HotPools()
	if len(hot) != 2 {
		t.Fatalf("hot class count: got %d want 2", len(hot))
	}
	for _, c := range hot {
		if c.Mode != ModeHotPool {
			t.Fatalf("hot class %s has mode %s", c.Name, c.Mode)
		}
		if c.PodConcurrency < 2 {
			t.Fatalf("hot class %s should process more than one job per pod", c.Name)
		}
	}

	cold := ColdJobs()
	if len(cold) != 5 {
		t.Fatalf("cold class count: got %d want 5", len(cold))
	}
	for _, c := range cold {
		if c.Mode != ModeColdJob {
			t.Fatalf("cold class %s has mode %s", c.Name, c.Mode)
		}
		if c.PodConcurrency != 1 {
			t.Fatalf("cold class %s should isolate one heavy subjob per pod", c.Name)
		}
	}
}

func TestByName(t *testing.T) {
	c, ok := ByName("scip-jvm")
	if !ok {
		t.Fatal("scip-jvm not found")
	}
	if c.Image != "codeintel-scip-jvm" {
		t.Fatalf("image: got %q", c.Image)
	}
	if _, ok := ByName("missing"); ok {
		t.Fatal("missing class returned ok=true")
	}
}

func TestSCIPWorkerClassCoverage(t *testing.T) {
	required := map[string]bool{
		"ts-js":     false,
		"python":    false,
		"go":        false,
		"jvm":       false,
		"dotnet":    false,
		"rust-dart": false,
		"cpp":       false,
		"ruby":      false,
	}
	for _, c := range All() {
		for _, wc := range c.SCIPWorkerClasses {
			if _, ok := required[wc]; ok {
				required[wc] = true
			}
		}
	}
	for wc, seen := range required {
		if !seen {
			t.Fatalf("SCIP worker class %q is not covered", wc)
		}
	}
}

func TestQueuesComeFromCentralRegistry(t *testing.T) {
	got := QueueNames()
	want := append([]string{}, asynqueues.ExecutorQueues()...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("queue count: got %d want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("queue[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestForSCIPWorkerClass(t *testing.T) {
	cases := []struct {
		scipWorkerClass string
		name            string
		queue           string
		mode            ExecutionMode
		architecture    string
	}{
		{"ts-js", "scip-ts-python", asynqueues.QueueIndexSCIPTSPython, ModeHotPool, "multi"},
		{"python", "scip-ts-python", asynqueues.QueueIndexSCIPTSPython, ModeHotPool, "multi"},
		{"go", "scip-go", asynqueues.QueueIndexSCIPGo, ModeHotPool, "multi"},
		{"jvm", "scip-jvm", asynqueues.QueueIndexSCIPJVM, ModeColdJob, "multi"},
		{"dotnet", "scip-dotnet", asynqueues.QueueIndexSCIPDotnet, ModeColdJob, "multi"},
		{"rust-dart", "scip-rust-dart", asynqueues.QueueIndexSCIPRustDart, ModeColdJob, "multi"},
		{"cpp", "scip-cpp-x86", asynqueues.QueueIndexSCIPCPPX86, ModeColdJob, "amd64"},
		{"ruby", "scip-ruby-x86", asynqueues.QueueIndexSCIPRubyX86, ModeColdJob, "amd64"},
	}
	for _, tc := range cases {
		t.Run(tc.scipWorkerClass, func(t *testing.T) {
			c, ok := ForSCIPWorkerClass(tc.scipWorkerClass)
			if !ok {
				t.Fatalf("%s SCIP worker class not mapped", tc.scipWorkerClass)
			}
			if c.Name != tc.name || c.QueueName != tc.queue || c.Mode != tc.mode || c.Architecture != tc.architecture {
				t.Fatalf("%s mapped to %+v", tc.scipWorkerClass, c)
			}
		})
	}
	if _, ok := ForSCIPWorkerClass("unknown"); ok {
		t.Fatal("unknown SCIP worker class returned ok=true")
	}
}

func TestDeploymentMatrixMatchesRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/worker-classes.example.yaml")
	if err != nil {
		t.Fatalf("read deployment matrix: %v", err)
	}
	var doc struct {
		WorkerClasses []struct {
			Name              string   `yaml:"name"`
			Mode              string   `yaml:"mode"`
			Image             string   `yaml:"image"`
			QueueName         string   `yaml:"queueName"`
			NodePool          string   `yaml:"nodePool"`
			SCIPWorkerClasses []string `yaml:"scipWorkerClasses"`
			SCIPIndexers      []string `yaml:"scipIndexers"`
			PodConcurrency    int      `yaml:"podConcurrency"`
			MaxPods           int      `yaml:"maxPods"`
			Architecture      string   `yaml:"architecture"`
		} `yaml:"workerClasses"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse deployment matrix: %v", err)
	}
	byName := map[string]WorkerClass{}
	for _, c := range All() {
		byName[c.Name] = c
	}
	if len(doc.WorkerClasses) != len(byName) {
		t.Fatalf("matrix class count = %d want %d", len(doc.WorkerClasses), len(byName))
	}
	for _, item := range doc.WorkerClasses {
		c, ok := byName[item.Name]
		if !ok {
			t.Fatalf("matrix has unknown class %q", item.Name)
		}
		if item.Mode != string(c.Mode) ||
			item.Image != c.Image ||
			item.QueueName != c.QueueName ||
			item.NodePool != c.NodePool ||
			item.PodConcurrency != c.PodConcurrency ||
			item.MaxPods != c.MaxPods {
			t.Fatalf("matrix drift for %s: yaml=%+v registry=%+v", item.Name, item, c)
		}
		if item.Architecture == "" {
			item.Architecture = "multi"
		}
		if item.Architecture != c.Architecture {
			t.Fatalf("architecture drift for %s: yaml=%s registry=%s", item.Name, item.Architecture, c.Architecture)
		}
		if !equalStrings(item.SCIPWorkerClasses, c.SCIPWorkerClasses) {
			t.Fatalf("SCIP worker class drift for %s: yaml=%v registry=%v", item.Name, item.SCIPWorkerClasses, c.SCIPWorkerClasses)
		}
		if !equalStrings(item.SCIPIndexers, c.SCIPIndexers) {
			t.Fatalf("SCIP indexer drift for %s: yaml=%v registry=%v", item.Name, item.SCIPIndexers, c.SCIPIndexers)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
