package claude

import (
	"strings"
	"sync"
)

// Antigravity (Claude-on-Vertex) validates tool_use.id against the pattern
// ^[a-zA-Z0-9_-]+$. Tool names may contain characters outside that class
// (most commonly '.' in namespaced MCP tools such as "default_api.foo"), which
// previously leaked into the generated id and triggered upstream HTTP 400:
//
//	messages.N.content.M.tool_use.id: String should match pattern '^[a-zA-Z0-9_-]+$'
//
// We sanitize the function name before embedding it in the id. Because the
// Claude tool_result block carries only tool_use_id (not the function name),
// the reverse path reconstructs the name by parsing the id. Sanitizing is not
// reversible, so we keep a process-wide id->name map populated at generation
// time and consulted when converting tool_result back to a functionResponse.

// toolUseNameByID maps a generated tool_use id to its original (unsanitized)
// function name so the reverse conversion can recover the exact name.
var toolUseNameByID sync.Map // map[string]string

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
// names. The character class is identical to sanitizeToolUseIDComponent; the
// distinct name documents intent at the two call sites.
func sanitizeToolUseID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return id
	}
	return sanitizeToolUseIDComponent(id)
}

// rememberToolUseName records the original function name for a generated id.
func rememberToolUseName(id, name string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	toolUseNameByID.Store(id, name)
}

// functionNameForToolUseID returns the original function name for a tool_use id.
//
// It first consults the generation-time map (exact, handles sanitized names),
// then falls back to the legacy heuristic of stripping the trailing
// "-<unixnano>-<counter>" suffix for ids produced before this change or by
// other proxies.
func functionNameForToolUseID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return id
	}
	if v, ok := toolUseNameByID.Load(id); ok {
		if name, ok := v.(string); ok && name != "" {
			return name
		}
	}
	// Legacy fallback: id == "<name>-<unixnano>-<counter>".
	parts := strings.Split(id, "-")
	if len(parts) > 2 {
		return strings.Join(parts[0:len(parts)-2], "-")
	}
	return id
}
