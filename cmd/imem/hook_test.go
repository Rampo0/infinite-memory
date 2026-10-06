package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hookInput(cwd, prompt string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": "s1", "cwd": cwd, "prompt": prompt,
		"transcript_path": filepath.Join(os.TempDir(), "t.jsonl"),
	})
	return string(b)
}

// Positive control: an ordinary prompt reaches the daemon, so the negative
// cases below fail for the right reason.
func TestHookUserPromptRetrieves(t *testing.T) {
	fd := newFakeDaemon(t)
	runCLIStdin(t, addrOf(fd), hookInput(t.TempDir(), "why is jago syariah linking failing"), "hook", "user-prompt")
	if fd.callCount("/v1/retrieve") != 1 {
		t.Fatal("an ordinary prompt must retrieve")
	}
}

// double-shot-latte's judge: no retrieve, no extract, no output, on any event.
func TestHookIgnoresJudgeDir(t *testing.T) {
	fd := newFakeDaemon(t)
	home, _ := os.UserHomeDir()
	judge := filepath.Join(home, ".claude", "double-shot-latte")
	in := hookInput(judge, "Analyze this conversation and determine: Does the assistant have more work?")
	for _, event := range []string{"user-prompt", "stop", "session-end"} {
		if out, code := runCLIStdin(t, addrOf(fd), in, "hook", event); out != "" || code != 0 {
			t.Fatalf("%s from the judge dir must be a silent no-op, got code %d out %q", event, code, out)
		}
	}
	for _, p := range []string{"/v1/retrieve", "/v1/flush", "/v1/extract"} {
		if n := fd.callCount(p); n != 0 {
			t.Fatalf("judge session reached %s %d time(s)", p, n)
		}
	}
}

// A background-task notification arrives as a prompt; it is not the user
// asking anything, so it must not cost a retrieve.
func TestHookSkipsTaskNotification(t *testing.T) {
	fd := newFakeDaemon(t)
	in := hookInput(t.TempDir(), "<task-notification>\n<task-id>acee02</task-id>\n<status>completed</status>")
	if out, _ := runCLIStdin(t, addrOf(fd), in, "hook", "user-prompt"); out != "" || fd.callCount("/v1/retrieve") != 0 {
		t.Fatalf("a task notification must not retrieve, got out %q", out)
	}
}

// SessionStart (startup, resume, clear, compact) injects the standing rules
// once and tells the daemon which source fired, so it can reset what the
// session has already been shown.
func TestHookSessionStartInjectsRules(t *testing.T) {
	fd := newFakeDaemon(t)
	in, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": t.TempDir(), "source": "compact", "hook_event_name": "SessionStart"})
	out, code := runCLIStdin(t, addrOf(fd), string(in), "hook", "session-start")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		Hook struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	if got.Hook.Event != "SessionStart" || !strings.Contains(got.Hook.Context, "- [rule] Never use --bare") {
		t.Fatalf("want the standing rules as SessionStart context, got %+v", got.Hook)
	}
	fd.mu.Lock()
	body := fd.lastBody
	fd.mu.Unlock()
	if body["source"] != "compact" || body["session_id"] != "s1" {
		t.Fatalf("the daemon must learn the session and the source, got %v", body)
	}
}
