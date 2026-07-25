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

func TestNewToolUseIDRoundTrip(t *testing.T) {
	// The id is self-describing: encoding then decoding recovers the exact
	// name — including dots, hyphens and unicode the legacy heuristic mangled —
	// with no shared state.
	names := []string{
		"default_api.read_file",
		"read",
		"foo.bar/baz:qux",
		"a-b-c",           // hyphens in the name: legacy suffix-strip got this wrong
		"инструмент",      // unicode
		"tool.with.dots.x",
	}
	for _, name := range names {
		id := newToolUseID(name)
		for _, r := range id {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
			if !ok {
				t.Fatalf("id %q for %q contains invalid rune %q", id, name, r)
			}
		}
		if got := functionNameForToolUseID(id); got != name {
			t.Fatalf("round trip %q: functionNameForToolUseID(%q) = %q", name, id, got)
		}
	}
}

func TestNewToolUseIDUnique(t *testing.T) {
	// The atomic counter keeps ids unique even for the same name generated in a
	// tight loop (guards the non-streaming path against id collisions).
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := newToolUseID("read")
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q at i=%d", id, i)
		}
		seen[id] = struct{}{}
	}
}

func TestFunctionNameForToolUseIDLegacyFallback(t *testing.T) {
	// Ids from older builds / other proxies ("<name>-<nano>-<counter>", where
	// the name segment is not valid hex) fall back to stripping the trailing
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
