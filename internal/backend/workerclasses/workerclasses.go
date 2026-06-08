// Package workerclasses defines backend-owned indexing executor
// classes used by the hybrid hot/cold worker fabric.
package workerclasses

import (
	"sort"

	"codeintel/pkg/asynqueues"
)

// ExecutionMode describes how a worker class should run under
// Kubernetes.
type ExecutionMode string

const (
	// ModeCore is the thin backend executor class for clone,
	// commit materialization, Zoekt, AST/tree-sitter scheduling,
	// and activation. It must not carry language SDKs.
	ModeCore ExecutionMode = "core"

	// ModeHotPool is a long-running Deployment that may process
	// multiple jobs per pod. Use it for frequent, relatively small
	// runtime classes that benefit from warm caches.
	ModeHotPool ExecutionMode = "hot-pool"

	// ModeColdJob is an ephemeral Job/ScaledJob class. Use it for
	// large SDKs, high memory variance, architecture-specific
	// binaries, or untrusted build-heavy indexers.
	ModeColdJob ExecutionMode = "cold-job"
)

// WorkerClass is the deployment and scheduling contract for one
// indexing executor class.
type WorkerClass struct {
	Name              string
	Mode              ExecutionMode
	QueueName         string
	Image             string
	NodePool          string
	SCIPWorkerClasses []string
	SCIPIndexers      []string
	PodConcurrency    int
	MaxPods           int
	CPURequest        string
	MemoryRequest     string
	CPULimit          string
	MemoryLimit       string
	Architecture      string
	Description       string
}

var classes = []WorkerClass{
	{
		Name:           "core",
		Mode:           ModeCore,
		QueueName:      asynqueues.QueueIndexCore,
		Image:          "codeintel-indexer-rs-core",
		NodePool:       "index-core",
		PodConcurrency: 2,
		MaxPods:        100,
		CPURequest:     "500m",
		MemoryRequest:  "1Gi",
		CPULimit:       "2",
		MemoryLimit:    "4Gi",
		Architecture:   "multi",
		Description:    "Backend-owned clone, commit snapshot, Zoekt, AST/tree-sitter scheduling, and activation without language SDKs.",
	},
	{
		Name:              "scip-ts-python",
		Mode:              ModeHotPool,
		QueueName:         asynqueues.QueueIndexSCIPTSPython,
		Image:             "codeintel-scip-ts-python",
		NodePool:          "index-light",
		SCIPWorkerClasses: []string{"ts-js", "python"},
		SCIPIndexers:      []string{"typescript", "python"},
		PodConcurrency:    4,
		MaxPods:           300,
		CPURequest:        "500m",
		MemoryRequest:     "1Gi",
		CPULimit:          "4",
		MemoryLimit:       "6Gi",
		Architecture:      "multi",
		Description:       "Warm pool for frequent TypeScript, JavaScript, and Python indexing.",
	},
	{
		Name:              "scip-go",
		Mode:              ModeHotPool,
		QueueName:         asynqueues.QueueIndexSCIPGo,
		Image:             "codeintel-scip-go",
		NodePool:          "index-light",
		SCIPWorkerClasses: []string{"go"},
		SCIPIndexers:      []string{"go"},
		PodConcurrency:    4,
		MaxPods:           200,
		CPURequest:        "500m",
		MemoryRequest:     "1Gi",
		CPULimit:          "4",
		MemoryLimit:       "6Gi",
		Architecture:      "multi",
		Description:       "Warm pool for Go indexing and module-aware SCIP extraction.",
	},
	{
		Name:              "scip-jvm",
		Mode:              ModeColdJob,
		QueueName:         asynqueues.QueueIndexSCIPJVM,
		Image:             "codeintel-scip-jvm",
		NodePool:          "index-heavy",
		SCIPWorkerClasses: []string{"jvm"},
		SCIPIndexers:      []string{"java"},
		PodConcurrency:    1,
		MaxPods:           100,
		CPURequest:        "1",
		MemoryRequest:     "4Gi",
		CPULimit:          "6",
		MemoryLimit:       "16Gi",
		Architecture:      "multi",
		Description:       "Ephemeral JVM worker for Maven, Gradle, and SBT projects.",
	},
	{
		Name:              "scip-dotnet",
		Mode:              ModeColdJob,
		QueueName:         asynqueues.QueueIndexSCIPDotnet,
		Image:             "codeintel-scip-dotnet",
		NodePool:          "index-heavy",
		SCIPWorkerClasses: []string{"dotnet"},
		SCIPIndexers:      []string{"dotnet"},
		PodConcurrency:    1,
		MaxPods:           100,
		CPURequest:        "1",
		MemoryRequest:     "4Gi",
		CPULimit:          "6",
		MemoryLimit:       "16Gi",
		Architecture:      "multi",
		Description:       "Ephemeral .NET worker for solution and project-file indexing.",
	},
	{
		Name:              "scip-rust-dart",
		Mode:              ModeColdJob,
		QueueName:         asynqueues.QueueIndexSCIPRustDart,
		Image:             "codeintel-scip-rust-dart",
		NodePool:          "index-heavy",
		SCIPWorkerClasses: []string{"rust-dart"},
		SCIPIndexers:      []string{"rust", "dart"},
		PodConcurrency:    1,
		MaxPods:           100,
		CPURequest:        "1",
		MemoryRequest:     "4Gi",
		CPULimit:          "6",
		MemoryLimit:       "16Gi",
		Architecture:      "multi",
		Description:       "Ephemeral worker for Cargo/rust-analyzer and Dart SCIP extraction.",
	},
	{
		Name:              "scip-cpp-x86",
		Mode:              ModeColdJob,
		QueueName:         asynqueues.QueueIndexSCIPCPPX86,
		Image:             "codeintel-scip-cpp",
		NodePool:          "index-native-x86",
		SCIPWorkerClasses: []string{"cpp"},
		SCIPIndexers:      []string{"clang"},
		PodConcurrency:    1,
		MaxPods:           50,
		CPURequest:        "2",
		MemoryRequest:     "6Gi",
		CPULimit:          "8",
		MemoryLimit:       "24Gi",
		Architecture:      "amd64",
		Description:       "Architecture-specific C/C++ worker for compile_commands.json projects.",
	},
	{
		Name:              "scip-ruby-x86",
		Mode:              ModeColdJob,
		QueueName:         asynqueues.QueueIndexSCIPRubyX86,
		Image:             "codeintel-scip-ruby",
		NodePool:          "index-native-x86",
		SCIPWorkerClasses: []string{"ruby"},
		SCIPIndexers:      []string{"ruby"},
		PodConcurrency:    1,
		MaxPods:           50,
		CPURequest:        "1",
		MemoryRequest:     "2Gi",
		CPULimit:          "4",
		MemoryLimit:       "8Gi",
		Architecture:      "amd64",
		Description:       "Architecture-specific Ruby worker until an arm64 toolchain is approved.",
	},
}

// All returns every governed class in deterministic order.
func All() []WorkerClass {
	out := make([]WorkerClass, len(classes))
	copy(out, classes)
	return out
}

// ByName returns the class with name.
func ByName(name string) (WorkerClass, bool) {
	for _, c := range classes {
		if c.Name == name {
			return c, true
		}
	}
	return WorkerClass{}, false
}

// ForSCIPWorkerClass maps the legacy SCIP workerClass value
// (`go`, `ts-js`, `rust-dart`, ...) to the governed Kubernetes
// executor class that should run it. The distinction matters:
// SCIP workerClass is language/tooling taxonomy, while
// WorkerClass.Name is the deployable queue/image contract.
func ForSCIPWorkerClass(scipWorkerClass string) (WorkerClass, bool) {
	for _, c := range classes {
		for _, wc := range c.SCIPWorkerClasses {
			if wc == scipWorkerClass {
				return c, true
			}
		}
	}
	return WorkerClass{}, false
}

// HotPools returns the classes that should run as scalable
// long-lived worker Deployments.
func HotPools() []WorkerClass {
	return filterByMode(ModeHotPool)
}

// ColdJobs returns the classes that should run as ephemeral
// Jobs/ScaledJobs.
func ColdJobs() []WorkerClass {
	return filterByMode(ModeColdJob)
}

func filterByMode(mode ExecutionMode) []WorkerClass {
	var out []WorkerClass
	for _, c := range classes {
		if c.Mode == mode {
			out = append(out, c)
		}
	}
	return out
}

// QueueNames returns every queue owned by the executor fabric.
func QueueNames() []string {
	out := make([]string, 0, len(classes))
	for _, c := range classes {
		out = append(out, c.QueueName)
	}
	sort.Strings(out)
	return out
}
