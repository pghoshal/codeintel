package asynqueues

import (
	"sort"
	"strings"
	"testing"
)

// TestQueueNames_LegacyParity locks the byte-for-byte queue
// identifiers against the legacy BullMQ QUEUE_NAME literals. A
// drift here breaks compatibility with any legacy producer still
// enqueuing under the old name OR any legacy worker still
// consuming.
func TestQueueNames_LegacyParity(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"connection-sync", QueueConnectionSync, "connection-sync-queue"},
		{"repo-index", QueueRepoIndex, "repo-index-queue"},
		{"code-graph-write", QueueCodeGraphWrite, "code-graph-write"},
		{"account-permission-sync", QueueAccountPermissionSync, "accountPermissionSyncQueue"},
		{"repo-permission-sync", QueueRepoPermissionSync, "repoPermissionSyncQueue"},
		{"llm-completion", QueueLLMCompletion, "codeintel-llm-completion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("byte-mismatch:\n got: %q\nwant: %q", tc.got, tc.want)
			}
		})
	}
}

// TestAll_StableDeterministicOrder confirms All() returns the
// four queue names in a stable order. The order matters because
// asynq Server config + dashboard labels honour it; a re-order
// is a wire-shape regression even if the set is unchanged.
func TestAll_StableDeterministicOrder(t *testing.T) {
	got := All()
	want := []string{
		"connection-sync-queue",
		"repo-index-queue",
		"code-graph-write",
		"accountPermissionSyncQueue",
		"repoPermissionSyncQueue",
		"codeintel-llm-completion",
	}
	if len(got) != len(want) {
		t.Fatalf("length: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q", i, got[i], want[i])
		}
	}
	// Stability across calls — defensive against an accidental
	// shuffle via map iteration.
	for i := 0; i < 5; i++ {
		if !equalSlices(All(), want) {
			t.Errorf("call %d shuffled: %v", i, All())
		}
	}
}

// TestAll_NoDuplicates confirms the set is a set — no duplicate
// queue names that would cause asynq Server.Queues to silently
// drop one.
func TestAll_NoDuplicates(t *testing.T) {
	seen := make(map[string]struct{})
	for _, q := range All() {
		if _, dup := seen[q]; dup {
			t.Errorf("duplicate queue name: %q", q)
		}
		seen[q] = struct{}{}
	}
}

// TestAll_NamesNonEmpty + reasonable shape — no whitespace, no
// asynq-reserved characters. The TS source's identifiers were
// hand-written; the test gates a future drift where someone
// types a typo or adds trailing whitespace.
func TestAll_NameShape(t *testing.T) {
	for _, q := range All() {
		if q == "" {
			t.Errorf("empty queue name in All()")
		}
		if strings.ContainsAny(q, " \t\n") {
			t.Errorf("queue name %q contains whitespace", q)
		}
		// asynq's internal Redis key separator is `:`; a queue
		// name containing it would collide with the key layout.
		if strings.Contains(q, ":") {
			t.Errorf("queue name %q contains the asynq Redis-key separator ':'", q)
		}
	}
}

// TestDefaultPriorities_MapShape locks the
// `map[QueueName]int` shape asynq.Server.Queues expects. Every
// queue gets priority 1 (matching BullMQ's no-priority default).
func TestDefaultPriorities_MapShape(t *testing.T) {
	prios := DefaultPriorities()
	if len(prios) != 6 {
		t.Errorf("map size: got %d, want 6", len(prios))
	}
	keys := make([]string, 0, len(prios))
	for k, v := range prios {
		if v != 1 {
			t.Errorf("priority for %q: got %d, want 1", k, v)
		}
		keys = append(keys, k)
	}
	// Confirm the map covers every queue in All().
	sort.Strings(keys)
	want := append([]string{}, All()...)
	sort.Strings(want)
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("priority map key set mismatch at %d: got %q, want %q", i, keys[i], want[i])
		}
	}
	if _, ok := prios[QueueRepoIndexRust]; ok {
		t.Errorf("Go DefaultPriorities must not subscribe to Rust-owned queue %q", QueueRepoIndexRust)
	}
	for _, q := range ExecutorQueues() {
		if _, ok := prios[q]; ok {
			t.Errorf("Go DefaultPriorities must not subscribe to executor queue %q", q)
		}
	}
}

func TestExecutorQueues_StableAndSeparate(t *testing.T) {
	got := ExecutorQueues()
	want := []string{
		"codeintel-index-core",
		"codeintel-index-scip-ts-python",
		"codeintel-index-scip-go",
		"codeintel-index-scip-jvm",
		"codeintel-index-scip-dotnet",
		"codeintel-index-scip-rust-dart",
		"codeintel-index-scip-cpp-x86",
		"codeintel-index-scip-ruby-x86",
	}
	if !equalSlices(got, want) {
		t.Fatalf("ExecutorQueues() = %v want %v", got, want)
	}
	goQueues := map[string]struct{}{}
	for _, q := range GoSubscribedQueues() {
		goQueues[q] = struct{}{}
	}
	for _, q := range got {
		if q == "" || strings.ContainsAny(q, " \t\n:") {
			t.Fatalf("bad executor queue shape: %q", q)
		}
		if _, ok := goQueues[q]; ok {
			t.Fatalf("executor queue %q leaked into Go backend subscriptions", q)
		}
	}
}

func equalSlices(a, b []string) bool {
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
