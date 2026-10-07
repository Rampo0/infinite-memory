package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeLines(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func userLine(text string) string {
	return `{"type":"user","message":{"role":"user","content":` + quote(text) + `}}`
}

func assistantLine(text string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":` + quote(text) + `}]}}`
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func TestReadChunkAlwaysTakesOneTurn(t *testing.T) {
	big := strings.Repeat("x", 500)
	p := writeLines(t, userLine(big), assistantLine("ok"))
	c, err := ReadChunk(p, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Turns) != 1 || c.Next != 1 {
		t.Fatalf("a turn over budget must still be taken alone: turns=%d next=%d", len(c.Turns), c.Next)
	}
}

func TestClipMessageKeepsHeadAndTailOnRuneBoundaries(t *testing.T) {
	text := "START " + strings.Repeat("é", 5000) + " END"
	got := clipMessage(text)
	if !utf8.ValidString(got) {
		t.Fatal("clip split a multi-byte character")
	}
	if !strings.HasPrefix(got, "START ") || !strings.HasSuffix(got, " END") {
		t.Fatalf("clip must keep the head and the tail, got %q…%q", got[:10], got[len(got)-10:])
	}
	if len(got) > messageClip+16 {
		t.Fatalf("clip too long: %d", len(got))
	}
}

func TestReadChunkKeepsLongMessagesWhole(t *testing.T) {
	long := strings.Repeat("a", 4000) + " decision at the end"
	p := writeLines(t, assistantLine(long))
	c, err := ReadChunk(p, 0, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Turns) != 1 || !strings.HasSuffix(c.Turns[0].Text, "decision at the end") {
		t.Fatalf("a 4KB reply must survive whole, got %d bytes", len(c.Turns[0].Text))
	}
}

func TestReadChunkKeepsSubagentReports(t *testing.T) {
	p := writeLines(t,
		`{"type":"assistant","message":{"role":"assistant","content":[`+
			`{"type":"tool_use","id":"ag1","name":"Agent","input":{"description":"Audit save path"}},`+
			`{"type":"tool_use","id":"rd1","name":"Read","input":{"file_path":"/x.go"}},`+
			`{"type":"tool_use","id":"ag2","name":"Agent","input":{"description":"Background audit"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"ag1","content":[{"type":"text","text":"Cursor skips short deltas."}]},`+
			`{"type":"tool_result","tool_use_id":"rd1","content":"package main secret"},`+
			`{"type":"tool_result","tool_use_id":"ag2","content":"Async agent launched successfully. agentId: x"}]}}`)
	c, err := ReadChunk(p, 0, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Turns) != 2 {
		t.Fatalf("want the tool turn plus one subagent report, got %+v", c.Turns)
	}
	rep := c.Turns[1]
	if rep.Role != "assistant" || rep.Text != "Subagent report: Cursor skips short deltas." {
		t.Fatalf("subagent report misread: %+v", rep)
	}
}

func TestReadChunkKeepsPromptsQueuedWhileBusy(t *testing.T) {
	p := writeLines(t,
		`{"type":"queue-operation","operation":"enqueue","content":"dont post the review yet"}`,
		`{"type":"attachment","attachment":{"type":"queued_command","prompt":"dont post the review yet"}}`,
		`{"type":"attachment","attachment":{"type":"hook_additional_context","content":["noise"]}}`)
	c, err := ReadChunk(p, 0, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Turns) != 1 || c.Turns[0].Role != "user" || c.Turns[0].Text != "dont post the review yet" {
		t.Fatalf("a prompt typed while busy is the user talking: %+v", c.Turns)
	}
}

func TestReadChunkKeepsSlashCommandArguments(t *testing.T) {
	p := writeLines(t,
		userLine("<command-message>reviewing-account-mr</command-message>\n<command-name>/reviewing-account-mr</command-name>\n"+
			"<command-args>https://gitlab/x/-/merge_requests/1014 dont post</command-args>"),
		userLine("<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>"))
	c, err := ReadChunk(p, 0, 24000)
	if err != nil {
		t.Fatal(err)
	}
	want := "/reviewing-account-mr https://gitlab/x/-/merge_requests/1014 dont post"
	if len(c.Turns) != 1 || c.Turns[0].Text != want {
		t.Fatalf("want only the command with arguments as %q, got %+v", want, c.Turns)
	}
}

func TestLastReplyMatches(t *testing.T) {
	p := writeLines(t, userLine("q"), assistantLine("Committed as bddae456.\n\nAll green."))
	if !HasFinalReply(p, "Committed as bddae456.  All green.") {
		t.Fatal("the written final reply must match despite whitespace differences")
	}
	if HasFinalReply(p, "MR created: !1020") {
		t.Fatal("a reply not yet written must not match")
	}
	if !HasFinalReply(p, "") {
		t.Fatal("no reply to wait for counts as present")
	}
}
