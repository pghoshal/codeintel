package redisclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	EnvRequireBoundedMemory    = "CODEINTEL_REDIS_REQUIRE_BOUNDED_MEMORY"
	EnvAllowedEvictionPolicies = "CODEINTEL_REDIS_ALLOWED_EVICTION_POLICIES"
)

var (
	ErrMemoryPolicyUnbounded = errors.New("redisclient: Redis maxmemory is not configured")
	ErrEvictionPolicyUnsafe  = errors.New("redisclient: Redis eviction policy is not allowed")
)

type OperationalPolicy struct {
	RequireBoundedMemory    bool
	AllowedEvictionPolicies map[string]bool
}

type OperationalSnapshot struct {
	MaxMemoryBytes  int64
	UsedMemoryBytes int64
	EvictionPolicy  string
	KeyCount        int64
	RawKeyspace     map[string]string
}

func LoadOperationalPolicyFromEnv() OperationalPolicy {
	return OperationalPolicy{
		RequireBoundedMemory:    strings.EqualFold(os.Getenv(EnvRequireBoundedMemory), "true"),
		AllowedEvictionPolicies: parseAllowedEvictionPolicies(os.Getenv(EnvAllowedEvictionPolicies)),
	}
}

func (p OperationalPolicy) WithDefaults() OperationalPolicy {
	if len(p.AllowedEvictionPolicies) == 0 {
		p.AllowedEvictionPolicies = parseAllowedEvictionPolicies("allkeys-lru,allkeys-lfu,volatile-lru,volatile-lfu")
	}
	return p
}

func (c *Client) InspectOperationalSnapshot(ctx context.Context) (OperationalSnapshot, error) {
	if c == nil || c.r == nil {
		return OperationalSnapshot{}, errors.New("redisclient: client is nil")
	}
	raw, err := c.r.Info(ctx, "memory", "keyspace").Result()
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("redisclient: INFO memory keyspace: %w", err)
	}
	return parseOperationalSnapshot(raw), nil
}

func (c *Client) EnforceOperationalPolicy(ctx context.Context, policy OperationalPolicy) (OperationalSnapshot, error) {
	snapshot, err := c.InspectOperationalSnapshot(ctx)
	if err != nil {
		return OperationalSnapshot{}, err
	}
	policy = policy.WithDefaults()
	if policy.RequireBoundedMemory {
		if snapshot.MaxMemoryBytes <= 0 {
			return snapshot, ErrMemoryPolicyUnbounded
		}
		if !policy.AllowedEvictionPolicies[strings.ToLower(snapshot.EvictionPolicy)] {
			return snapshot, fmt.Errorf("%w: %s", ErrEvictionPolicyUnsafe, snapshot.EvictionPolicy)
		}
	}
	return snapshot, nil
}

func parseOperationalSnapshot(raw string) OperationalSnapshot {
	out := OperationalSnapshot{RawKeyspace: map[string]string{}}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "maxmemory":
			out.MaxMemoryBytes = parseRedisInt(value)
		case "used_memory":
			out.UsedMemoryBytes = parseRedisInt(value)
		case "maxmemory_policy":
			out.EvictionPolicy = strings.ToLower(value)
		default:
			if strings.HasPrefix(key, "db") {
				out.RawKeyspace[key] = value
				out.KeyCount += parseKeyspaceKeys(value)
			}
		}
	}
	return out
}

func parseAllowedEvictionPolicies(raw string) map[string]bool {
	out := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item != "" {
			out[item] = true
		}
	}
	return out
}

func parseRedisInt(raw string) int64 {
	value, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return value
}

func parseKeyspaceKeys(raw string) int64 {
	for _, part := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && key == "keys" {
			return parseRedisInt(value)
		}
	}
	return 0
}
