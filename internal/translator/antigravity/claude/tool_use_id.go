package claude

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Antigravity (Claude-on-Vertex) validates tool_use.id against the pattern
// ^[a-zA-Z0-9_-]+$. Tool names may contain characters outside that class
// (most commonly '.' in namespaced MCP tools such as "default_api.foo"), which
// previously leaked into the generated id and triggered upstream HTTP 400:
//
//	messages.N.content.M.tool_use.id: String should match pattern '^[a-zA-Z0-9_-]+$'
//
// The Claude tool_result block carries only tool_use_id (not the function
// name), so the reverse path must recover the name from the id alone. Instead
// of keeping a process-wide id->name map (which grows without bound for a
// long-lived proxy), the id is self-describing: the original name is
// hex-encoded into it. Hex uses only [0-9a-f], which contains no '-', so it
// never collides with the '-' delimiter and always satisfies the id pattern.

// toolUseIDCounter provides a process-wide unique counter for tool use
// identifiers (guarantees uniqueness even within the same nanosecond).
var toolUseIDCounter uint64

// newToolUseID builds a unique, self-describing tool_use id of the form
// "<hexname>-<unixnano>-<counter>". functionNameForToolUseID reverses it with
// no shared state.
func newToolUseID(name string) string {
	return fmt.Sprintf("%s-%d-%d",
		hex.EncodeToString([]byte(name)),
		time.Now().UnixNano(),
		atomic.AddUint64(&toolUseIDCounter, 1),
	)
}

// sanitizeToolUseIDComponent replaces every character outside [a-zA-Z0-9_-]
// with '_' so the assembled id satisfies the Antigravity id pattern.
func sanitizeToolUseIDComponent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}

// sanitizeToolUseID cleans a complete tool_use id (as opposed to a single
// name component) so it satisfies ^[a-zA-Z0-9_-]+$. It is applied on the
// request path to ids echoed back by the client, which may have been produced
// before id sanitization existed or by clients that embed unsanitized tool
// names. Ids produced by newToolUseID are already within the class, so this is
// a no-op for them.
func sanitizeToolUseID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return id
	}
	return sanitizeToolUseIDComponent(id)
}

// functionNameForToolUseID returns the original function name for a tool_use id.
//
// New ids ("<hexname>-<unixnano>-<counter>") are decoded directly from the
// hex-encoded name segment. Ids produced by older builds or other proxies fall
// back to the legacy heuristic of stripping the trailing "-<unixnano>-<counter>"
// suffix.
func functionNameForToolUseID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return id
	}
	// New self-describing format: the hex-encoded name is the segment before
	// the first '-'. Decode only when it is well-formed hex that yields valid
	// UTF-8, so a legacy name that coincidentally looks like hex (e.g. "cafe")
	// is not misread — such bytes almost never decode to valid UTF-8.
	// ponytail: hex round-trips the exact name (dots, hyphens, unicode) with
	// zero state; the UTF-8 gate is the only ambiguity vs legacy ids.
	if i := strings.IndexByte(id, '-'); i > 0 {
		if b, err := hex.DecodeString(id[:i]); err == nil && utf8.Valid(b) {
			return string(b)
		}
	}
	// Legacy fallback: id == "<name>-<unixnano>-<counter>".
	parts := strings.Split(id, "-")
	if len(parts) > 2 {
		return strings.Join(parts[0:len(parts)-2], "-")
	}
	return id
}
