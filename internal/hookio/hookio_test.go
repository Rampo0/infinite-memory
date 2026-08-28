package hookio

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func emit(t *testing.T, context, msg string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if err := EmitContext(&buf, "UserPromptSubmit", context, msg); err != nil {
		t.Fatalf("emit: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON %q: %v", buf.String(), err)
	}
	return out
}

func TestEmitContextBoth(t *testing.T) {
	out := emit(t, "block text", "imem: 2 memories + 1 rules (5ms)")
	if out["systemMessage"] != "imem: 2 memories + 1 rules (5ms)" {
		t.Fatalf("systemMessage wrong: %#v", out["systemMessage"])
	}
	hso, ok := out["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("hookSpecificOutput missing: %#v", out)
	}
	if hso["hookEventName"] != "UserPromptSubmit" || hso["additionalContext"] != "block text" {
		t.Fatalf("envelope wrong: %#v", hso)
	}
}

// A message-only emit (daemon down, nothing to inject) must not ship an empty
// additionalContext envelope.
func TestEmitContextMessageOnly(t *testing.T) {
	out := emit(t, "", "imem: daemon unreachable — memory off")
	if _, ok := out["hookSpecificOutput"]; ok {
		t.Fatalf("want no hookSpecificOutput, got %#v", out)
	}
	if out["systemMessage"] != "imem: daemon unreachable — memory off" {
		t.Fatalf("systemMessage wrong: %#v", out["systemMessage"])
	}
}

func TestEmitContextSilent(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitContext(&buf, "UserPromptSubmit", "", ""); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "{}" {
		t.Fatalf("want empty object, got %q", got)
	}
}

func TestPromptTextFallback(t *testing.T) {
	in, err := Read(strings.NewReader(`{"prompt":"old field"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if in.PromptText() != "old field" {
		t.Fatalf("fallback failed: %q", in.PromptText())
	}
	in, err = Read(strings.NewReader(`{"user_prompt":"new","prompt":"old"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if in.PromptText() != "new" {
		t.Fatalf("user_prompt must win, got %q", in.PromptText())
	}
}
