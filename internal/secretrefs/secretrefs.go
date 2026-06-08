// Package secretrefs walks JSON-decoded config trees to find and
// rewrite secret references. The codeintel HTTP layer uses these
// helpers to (a) refuse deleting a secret while a live config still
// references it, (b) stamp the org id on every reference before
// persistence, and (c) redact sensitive sibling fields from a
// config before echoing it to a client.
//
// The set of recognised secret-kind discriminators is extensible
// via the Kind interface. Built-in kinds are registered in init();
// downstream packages register more by calling Register at startup.
package secretrefs

import (
	"strings"
	"sync"
)

// Kind describes one secret-reference shape the walker recognises.
// Implementations are stateless and safe for concurrent use.
//
// Discriminator: the JSON object key whose presence (and string
// value) identifies the kind. For the built-in "secretRef" kind
// the discriminator is the literal "secretRef" key.
//
// Redact: returns the redacted form of a record matching this kind
// — typically `{<discriminator>: <value>}` with every sibling
// stripped.
type Kind interface {
	Discriminator() string
	Redact(record map[string]any) map[string]any
}

// registry holds every Kind registered with the package. Mutations
// happen at process startup (init() + Register); reads happen on
// every walker invocation and are lock-free via a snapshot.
type registry struct {
	mu    sync.RWMutex
	kinds []Kind
}

var defaultRegistry = &registry{}

// Register installs a new Kind so the walker recognises records
// matching its discriminator. Safe to call multiple times; later
// kinds match in registration order.
func Register(k Kind) {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	defaultRegistry.kinds = append(defaultRegistry.kinds, k)
}

// kindsSnapshot returns the current registry contents under a read
// lock. The slice is safe to read concurrently — the registry
// promises never to mutate it in place.
func kindsSnapshot() []Kind {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()
	return defaultRegistry.kinds
}

// SecretRefKind is the built-in `secretRef` discriminator: a record
// of shape `{"secretRef":"<key>"}` references an org secret by name.
type SecretRefKind struct{}

func (SecretRefKind) Discriminator() string { return "secretRef" }
func (SecretRefKind) Redact(record map[string]any) map[string]any {
	v, _ := record["secretRef"].(string)
	return map[string]any{"secretRef": v}
}

// EnvKind is the built-in `env` discriminator: a record of shape
// `{"env":"<VAR>"}` references an environment variable.
type EnvKind struct{}

func (EnvKind) Discriminator() string { return "env" }
func (EnvKind) Redact(record map[string]any) map[string]any {
	v, _ := record["env"].(string)
	return map[string]any{"env": v}
}

// GoogleCloudSecretKind is the built-in `googleCloudSecret`
// discriminator: a record of shape
// `{"googleCloudSecret":"projects/<p>/secrets/<n>/versions/<v>"}`
// references a Google Cloud Secret Manager secret.
type GoogleCloudSecretKind struct{}

func (GoogleCloudSecretKind) Discriminator() string { return "googleCloudSecret" }
func (GoogleCloudSecretKind) Redact(record map[string]any) map[string]any {
	v, _ := record["googleCloudSecret"].(string)
	return map[string]any{"googleCloudSecret": v}
}

func init() {
	Register(SecretRefKind{})
	Register(EnvKind{})
	Register(GoogleCloudSecretKind{})
}

// Collect recurses through a JSON-decoded value and returns every
// nested secretRef string it finds.
//
// Dedup semantics:
//   - Array items are deduped via a set: identical refs in the same
//     array collapse to one entry.
//   - Object-sibling duplicates are KEPT — two keys in the same
//     parent object both carrying the same value yield two entries.
//     This matches the documented object-spread semantics.
//
// Non-record / non-array values return an empty slice.
func Collect(value any) []string {
	switch v := value.(type) {
	case []any:
		seen := make(map[string]struct{})
		out := make([]string, 0)
		for _, item := range v {
			for _, ref := range Collect(item) {
				if _, dup := seen[ref]; dup {
					continue
				}
				seen[ref] = struct{}{}
				out = append(out, ref)
			}
		}
		return out
	case map[string]any:
		out := make([]string, 0)
		if s, ok := v["secretRef"].(string); ok {
			out = append(out, s)
		}
		for _, child := range v {
			out = append(out, Collect(child)...)
		}
		return out
	default:
		return nil
	}
}

// Contains reports whether the supplied key appears anywhere in the
// value's nested secretRef references.
func Contains(value any, key string) bool {
	for _, ref := range Collect(value) {
		if ref == key {
			return true
		}
	}
	return false
}

// CollectUnique walks the value and returns the distinct set of
// secretRef strings — array-and-sibling dedup applied at the top
// level. Useful for the missing-secret-ref check where one round-
// trip should handle the full candidate set.
func CollectUnique(value any) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, ref := range Collect(value) {
		if _, dup := seen[ref]; dup {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	return out
}

// MaxWalkDepth bounds the recursion depth of the walker functions
// in this package. A deeply-nested JSON tree (e.g. an attacker
// sending `{"a":{"a":{"a":...}}}` 10000 levels deep) would
// otherwise exhaust the Go goroutine stack. 64 levels is well
// above any plausible legitimate config nesting.
const MaxWalkDepth = 64

// BindToOrg rewrites every `{secretRef:"<key>"}` record in the
// value tree to `{secretRef:"<key>", orgId:<orgID>}`. The resulting
// tree is persisted to model / connection config columns so
// downstream resolvers can look up the secret without re-deriving
// the org scope.
//
// The input tree is NOT mutated — the returned tree is a fresh
// copy. Other recognised kinds (env, googleCloudSecret) pass
// through unchanged since they have no org scope to attach.
//
// Stack-safe: traversal depth is capped at MaxWalkDepth. Beyond
// that depth the returned tree truncates with the original value
// at the cutoff — never panics, never exhausts the stack.
func BindToOrg(value any, orgID int32) any {
	return bindToOrgAt(value, orgID, 0)
}

func bindToOrgAt(value any, orgID int32, depth int) any {
	if depth >= MaxWalkDepth {
		return value
	}
	switch v := value.(type) {
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, bindToOrgAt(item, orgID, depth+1))
		}
		return out
	case map[string]any:
		if s, ok := v["secretRef"].(string); ok {
			return map[string]any{
				"secretRef": s,
				// JSON unmarshalling produces float64 for numbers;
				// store orgId in the same shape so the result tree
				// round-trips through encoding/json identically.
				"orgId": float64(orgID),
			}
		}
		out := make(map[string]any, len(v))
		for k, child := range v {
			out[k] = bindToOrgAt(child, orgID, depth+1)
		}
		return out
	default:
		return value
	}
}

// Redact rewrites a config tree so only the registered secret-kind
// discriminators survive — the actual value of any
// recognised-kind record is preserved, every sibling is stripped.
//
// The set of recognised kinds is the registry (built-in:
// secretRef, env, googleCloudSecret; downstream packages add more
// via Register). The input tree is not mutated.
func Redact(value any) any {
	kinds := kindsSnapshot()
	return redactWith(value, kinds)
}

func redactWith(value any, kinds []Kind) any {
	return redactWithAt(value, kinds, 0)
}

func redactWithAt(value any, kinds []Kind, depth int) any {
	if depth >= MaxWalkDepth {
		return value
	}
	switch v := value.(type) {
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, redactWithAt(item, kinds, depth+1))
		}
		return out
	case map[string]any:
		// First-matching-kind wins; the registry preserves
		// registration order so callers can give precedence by
		// registering more-specific kinds before less-specific ones.
		for _, k := range kinds {
			if _, ok := v[k.Discriminator()].(string); ok {
				return k.Redact(v)
			}
		}
		out := make(map[string]any, len(v))
		for kk, child := range v {
			redactedChild := redactWithAt(child, kinds, depth+1)
			if isSensitiveConfigKey(kk) && !isRecognisedSecretKind(redactedChild, kinds) {
				out[kk] = "[REDACTED]"
				continue
			}
			out[kk] = redactedChild
		}
		return out
	default:
		return value
	}
}

func isRecognisedSecretKind(value any, kinds []Kind) bool {
	record, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, k := range kinds {
		if _, ok := record[k.Discriminator()].(string); ok {
			return true
		}
	}
	return false
}

func isSensitiveConfigKey(key string) bool {
	normalised := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", ""))
	switch normalised {
	case "token",
		"accesstoken",
		"refreshtoken",
		"password",
		"apikey",
		"clientsecret",
		"privatekey",
		"secret",
		"hardcodedsecret":
		return true
	default:
		return false
	}
}
