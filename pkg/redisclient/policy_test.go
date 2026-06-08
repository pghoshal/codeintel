package redisclient

import (
	"errors"
	"testing"
)

func TestParseOperationalSnapshotReadsMemoryPolicyAndKeyspace(t *testing.T) {
	raw := "# Memory\nused_memory:1048576\nmaxmemory:268435456\nmaxmemory_policy:allkeys-lfu\n# Keyspace\ndb0:keys=12,expires=4,avg_ttl=1000\ndb2:keys=3,expires=0,avg_ttl=0\n"
	got := parseOperationalSnapshot(raw)
	if got.UsedMemoryBytes != 1048576 {
		t.Fatalf("used memory = %d", got.UsedMemoryBytes)
	}
	if got.MaxMemoryBytes != 268435456 {
		t.Fatalf("maxmemory = %d", got.MaxMemoryBytes)
	}
	if got.EvictionPolicy != "allkeys-lfu" {
		t.Fatalf("eviction policy = %q", got.EvictionPolicy)
	}
	if got.KeyCount != 15 {
		t.Fatalf("key count = %d", got.KeyCount)
	}
}

func TestOperationalPolicyDefaultsAllowBoundedEvictionPolicies(t *testing.T) {
	policy := OperationalPolicy{RequireBoundedMemory: true}.WithDefaults()
	for _, want := range []string{"allkeys-lru", "allkeys-lfu", "volatile-lru", "volatile-lfu"} {
		if !policy.AllowedEvictionPolicies[want] {
			t.Fatalf("default allowed policies missing %s: %+v", want, policy.AllowedEvictionPolicies)
		}
	}
	if policy.AllowedEvictionPolicies["noeviction"] {
		t.Fatalf("noeviction must not be an allowed production default")
	}
}

func TestLoadOperationalPolicyFromEnv(t *testing.T) {
	t.Setenv(EnvRequireBoundedMemory, "true")
	t.Setenv(EnvAllowedEvictionPolicies, "allkeys-lru,volatile-ttl")
	policy := LoadOperationalPolicyFromEnv()
	if !policy.RequireBoundedMemory {
		t.Fatalf("RequireBoundedMemory should be true")
	}
	if !policy.AllowedEvictionPolicies["allkeys-lru"] || !policy.AllowedEvictionPolicies["volatile-ttl"] {
		t.Fatalf("allowed policies not parsed: %+v", policy.AllowedEvictionPolicies)
	}
}

func TestRedisPolicyErrorsAreSentinels(t *testing.T) {
	if !errors.Is(ErrMemoryPolicyUnbounded, ErrMemoryPolicyUnbounded) {
		t.Fatalf("unbounded memory sentinel should be errors.Is-compatible")
	}
	if !errors.Is(ErrEvictionPolicyUnsafe, ErrEvictionPolicyUnsafe) {
		t.Fatalf("unsafe eviction sentinel should be errors.Is-compatible")
	}
}
