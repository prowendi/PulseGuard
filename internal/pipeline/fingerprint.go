// Package pipeline implements the ingest -> outbox -> worker -> cleanup
// stages of the PulseGuard push pipeline.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Fingerprint returns a stable string identifying the payload for
// deduplication.
//
//   - When dedupKey is non-empty, it is returned verbatim — the user
//     explicitly told us how to bucket this push.
//   - Otherwise a canonical JSON serialisation (keys sorted recursively)
//     is hashed with SHA-256 and hex-encoded. This guarantees that two
//     payloads with the same content but different key ordering map to
//     the same fingerprint.
func Fingerprint(payload map[string]any, dedupKey string) string {
	if dedupKey != "" {
		return dedupKey
	}
	canon := canonicalise(payload)
	raw, err := json.Marshal(canon)
	if err != nil {
		// json.Marshal on a normalised map cannot fail in practice; even
		// if it did, the empty hash is safer than panicking.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// canonicalise rewrites the input so json.Marshal yields a stable byte
// sequence regardless of map key iteration order.
func canonicalise(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// Use an array of [key, value] pairs so JSON encoder preserves
		// the order we sorted.
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, []any{k, canonicalise(x[k])})
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canonicalise(e)
		}
		return out
	default:
		return v
	}
}
