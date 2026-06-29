package claude

import "testing"

func TestSanitizeToolUseIDComponentMatchesPattern(t *testing.T) {
	cases := map[string]string{
		"default_api.read_file": "default_api_read_file",
		"foo.bar/baz:qux":       "foo_bar_baz_qux",
		"already-valid_NAME9":   "already-valid_NAME9",
		"":                      "tool",
		"...":                   "___",
	}
	for in, want := range cases {
		if got := sanitizeToolUseIDComponent(in); got != want {
			t.Errorf("sanitizeToolUseIDComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeToolUseIDKeepsHyphenSuffix(t *testing.T) {
	// A generated id "<name>-<nano>-<counter>" with a dotted name must end up
	// matching ^[a-zA-Z0-9_-]+$ while preserving the trailing numeric segments.
	id := "default_api.foo-1700000000000000000-7"
	got := sanitizeToolUseID(id)
	want := "default_api_foo-1700000000000000000-7"
	if got != want {
		t.Fatalf("sanitizeToolUseID(%q) = %q, want %q", id, got, want)
	}
	for _, r := range got {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			t.Fatalf("sanitized id %q contains invalid rune %q", got, r)
		}
	}
}

func TestFunctionNameForToolUseIDUsesRememberedName(t *testing.T) {
	// Generation-time map wins: recovers the exact (dotted) name even though
	// the id itself was sanitized.
	id := "default_api_read_file-1700000000000000000-3"
	rememberToolUseName(id, "default_api.read_file")
	if got := functionNameForToolUseID(id); got != "default_api.read_file" {
		t.Fatalf("functionNameForToolUseID(%q) = %q, want default_api.read_file", id, got)
	}
}

func TestFunctionNameForToolUseIDLegacyFallback(t *testing.T) {
	// Unknown id (not in the map) falls back to stripping the trailing
	// "-<nano>-<counter>" suffix, matching pre-change behavior.
	id := "my-tool-1700000000000000000-9"
	if got := functionNameForToolUseID(id); got != "my-tool" {
		t.Fatalf("functionNameForToolUseID(%q) = %q, want my-tool", id, got)
	}
	// A bare id with no suffix returns itself.
	if got := functionNameForToolUseID("tool_42"); got != "tool_42" {
		t.Fatalf("functionNameForToolUseID(tool_42) = %q, want tool_42", got)
	}
}
