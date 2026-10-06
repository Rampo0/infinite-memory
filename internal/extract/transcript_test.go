package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("testdata/sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadDeltaFull(t *testing.T) {
	turns, total, err := ReadDelta(fixture(t), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 9 {
		t.Fatalf("want 9 total lines, got %d", total)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 conversational turns, got %d: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("unexpected roles: %+v", turns)
	}
	if turns[1].Text != "Registered port 7690 as the daemon HTTP address." {
		t.Fatalf("unexpected assistant text: %q", turns[1].Text)
	}
}

func TestReadDeltaCursor(t *testing.T) {
	turns, total, err := ReadDelta(fixture(t), 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 9 {
		t.Fatalf("total should still be 9, got %d", total)
	}
	if len(turns) != 1 || turns[0].Role != "assistant" {
		t.Fatalf("cursor=2 should yield only the assistant turn, got %+v", turns)
	}
}

func TestReadDeltaBudgetKeepsNewest(t *testing.T) {
	turns, _, err := ReadDelta(fixture(t), 0, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Role != "assistant" {
		t.Fatalf("budget should keep only the newest turn, got %+v", turns)
	}
}

func TestReadDeltaMissingFile(t *testing.T) {
	if _, _, err := ReadDelta("/nonexistent/nope.jsonl", 0, 0); err == nil {
		t.Fatal("want error for missing file")
	}
}

// agentkit.imem.save (ai-review, on-call) writes its transcripts by hand: a
// user line with a plain-string context, then assistant text blocks. Both
// must come through as turns, or every bot save extracts from nothing.
func TestReadDeltaAgentkitFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ai-review-1014-04833db1afd6-dfb1a93d.jsonl")
	data := `{"type": "user", "message": {"role": "user", "content": "ai-review comments on MR !1014"}}` + "\n" +
		`{"type": "assistant", "message": {"role": "assistant", "content": [{"type": "text", "text": "Missing nil check before Deref."}]}}` + "\n" +
		`{"type": "assistant", "message": {"role": "assistant", "content": [{"type": "text", "text": "Second comment chunk."}]}}` + "\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, total, err := ReadDelta(p, 0, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(turns) != 3 {
		t.Fatalf("want 3 lines and 3 turns, got %d lines, %+v", total, turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "ai-review comments on MR !1014" ||
		turns[2].Role != "assistant" || turns[2].Text != "Second comment chunk." {
		t.Fatalf("agentkit turns misread: %+v", turns)
	}
}

// C5: what the assistant DID (files touched, commands run) is evidence for
// reference memories. Tool calls become one-line summaries — a file path, a
// Bash description — and never the command itself, which may carry secrets.
func TestReadDeltaToolSummaries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	data := `{"type":"assistant","message":{"role":"assistant","content":[` +
		`{"type":"text","text":"Fixing the guard."},` +
		`{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/repo/usecase/guard.go","old_string":"a","new_string":"b"}},` +
		`{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"curl -H 'X-VAULT-TOKEN: s.SECRET123' https://vault","description":"Pull staging config from vault"}},` +
		`{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"echo s.SECRET456"}},` +
		`{"type":"tool_use","id":"t4","name":"mcp__postgres__query","input":{"sql":"select secret from x"}}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t5","name":"Read","input":{"file_path":"/repo/config.go"}}]}}` + "\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, _, err := ReadDelta(p, 0, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("a tool-only assistant message is still a turn: got %d", len(turns))
	}
	want := "Edit /repo/usecase/guard.go|Bash: Pull staging config from vault|Bash|mcp__postgres__query"
	if got := strings.Join(turns[0].Tools, "|"); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	for _, tr := range turns {
		all := tr.Text + strings.Join(tr.Tools, " ")
		if strings.Contains(all, "SECRET") || strings.Contains(all, "select secret") {
			t.Fatalf("tool inputs other than paths/descriptions must never leak: %q", all)
		}
	}
	if TotalChars(turns) != len("Fixing the guard.") {
		t.Fatalf("the spawn threshold counts conversation text only, got %d", TotalChars(turns))
	}
}

func TestBuildPromptRendersTools(t *testing.T) {
	p := BuildPrompt("/p", nil, []Turn{{Role: "assistant", Text: "Done.", Tools: []string{"Edit /repo/a.go", "Bash: Run tests"}}})
	if !strings.Contains(p, "[ASSISTANT] Done.\n  (tools: Edit /repo/a.go; Bash: Run tests)") {
		t.Fatalf("tools must follow the turn's text:\n%s", p)
	}
}
