package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func hookContext(t *testing.T, out string) (event, context string) {
	t.Helper()
	var got struct {
		Hook struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	return got.Hook.Event, got.Hook.Context
}

func TestHookUserPromptAlwaysCarriesTheProtocol(t *testing.T) {
	fd := newFakeDaemon(t)
	for addr, label := range map[string]string{addrOf(fd): "no matches", "127.0.0.1:1": "daemon down"} {
		out, _ := runCLIStdin(t, addr, hookInput(t.TempDir(), "lanjut"), "hook", "user-prompt")
		if event, ctx := hookContext(t, out); event != "UserPromptSubmit" || !strings.Contains(ctx, "<imem-protocol>") {
			t.Fatalf("%s: the self-search protocol must reach the model, got %q %q", label, event, ctx)
		}
	}
}

func TestHookUserPromptSendsTheLastExchange(t *testing.T) {
	fd := newFakeDaemon(t)
	path := filepath.Join(t.TempDir(), "t.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":"verify the imem retrieval spec"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hooks inject memories"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": t.TempDir(), "prompt": "lanjut", "transcript_path": path})
	runCLIStdin(t, addrOf(fd), string(in), "hook", "user-prompt")
	if ctx, _ := fd.body("/v1/retrieve")["context"].(string); ctx != "verify the imem retrieval spec\nhooks inject memories" {
		t.Fatalf("the retrieve must carry the last exchange, got %q", ctx)
	}
}

func TestHookStopHeadlessNeverBlocks(t *testing.T) {
	for _, env := range [][]string{{"CLAUDE_CODE_SESSION_ATTENDED=0"}, {"CLAUDE_CODE_ENTRYPOINT=sdk-cli"}, {"IMEM_AGENT=ai-review"}} {
		fd := newFakeDaemon(t)
		runCLIWith(t, cliCall{addr: addrOf(fd), stdin: hookInput(t.TempDir(), ""), env: env}, "hook", "stop")
		if fd.callCount("/v1/flush") != 0 || fd.callCount("/v1/extract") != 1 || fd.body("/v1/extract")["agent"] != true {
			t.Fatalf("%v: a headless stop must queue an agent extract without flushing, flush=%d extract=%d body=%v",
				env, fd.callCount("/v1/flush"), fd.callCount("/v1/extract"), fd.body("/v1/extract"))
		}
	}
}

func TestHookStopInteractiveFlushesAsTheUser(t *testing.T) {
	fd := newFakeDaemon(t)
	runCLIStdin(t, addrOf(fd), hookInput(t.TempDir(), ""), "hook", "stop")
	if fd.callCount("/v1/flush") != 1 || fd.body("/v1/flush")["agent"] != nil {
		t.Fatalf("an interactive stop flushes as the user, got flush=%d body=%v", fd.callCount("/v1/flush"), fd.body("/v1/flush"))
	}
}

func TestHookSubagentStartInjectsRulesAndProtocol(t *testing.T) {
	fd := newFakeDaemon(t)
	in, _ := json.Marshal(map[string]any{"session_id": "parent", "cwd": t.TempDir(), "agent_type": "Explore", "hook_event_name": "SubagentStart"})
	out, _ := runCLIStdin(t, addrOf(fd), string(in), "hook", "subagent-start")
	event, ctx := hookContext(t, out)
	if event != "SubagentStart" || !strings.Contains(ctx, "- [rule] Never use --bare") || !strings.Contains(ctx, "<imem-protocol>") {
		t.Fatalf("a subagent needs the standing rules and the protocol, got %q %q", event, ctx)
	}
	fd.mu.Lock()
	body := fd.lastBody
	fd.mu.Unlock()
	if body["source"] != "subagent" || body["session_id"] != "parent" {
		t.Fatalf("the daemon must learn it is a subagent of the parent session, got %v", body)
	}
}

func stopInput(t *testing.T, transcript, reply string, active bool) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"session_id": "s1", "cwd": t.TempDir(), "transcript_path": transcript,
		"last_assistant_message": reply, "stop_hook_active": active,
	})
	return string(b)
}

func transcriptWith(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const promptLine = `{"type":"user","message":{"role":"user","content":"commit it"}}`
const replyLine = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Committed as bddae456."}]}}`

func TestHookStopWaitsForTheFinalReplyBeforeFlushing(t *testing.T) {
	fd := newFakeDaemon(t)
	p := transcriptWith(t, promptLine)
	written := make(chan time.Time, 1)
	go func() {
		time.Sleep(400 * time.Millisecond)
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
		_, _ = f.WriteString(replyLine + "\n")
		_ = f.Close()
		written <- time.Now()
	}()
	runCLIStdin(t, addrOf(fd), stopInput(t, p, "Committed as bddae456.", false), "hook", "stop")
	at := <-written
	if fd.callCount("/v1/flush") != 1 || fd.calledAt("/v1/flush").Before(at) {
		t.Fatal("the flush must read the transcript only after the final reply is written")
	}
}

func TestHookStopDoesNotWaitWhenTheReplyIsWritten(t *testing.T) {
	fd := newFakeDaemon(t)
	p := transcriptWith(t, promptLine, replyLine)
	start := time.Now()
	runCLIStdin(t, addrOf(fd), stopInput(t, p, "Committed as bddae456.", false), "hook", "stop")
	if time.Since(start) > 2*time.Second || fd.callCount("/v1/flush") != 1 {
		t.Fatal("a reply already on disk must flush at once")
	}
}

func TestHookStopContinuationStillSaves(t *testing.T) {
	fd := newFakeDaemon(t)
	p := transcriptWith(t, promptLine, replyLine)
	runCLIStdin(t, addrOf(fd), stopInput(t, p, "Committed as bddae456.", true), "hook", "stop")
	if fd.callCount("/v1/extract") != 1 || fd.callCount("/v1/flush") != 0 {
		t.Fatalf("a continued stop must queue a save without blocking: extract=%d flush=%d",
			fd.callCount("/v1/extract"), fd.callCount("/v1/flush"))
	}
}

func TestHookSlashCommandRetrievesOnItsArguments(t *testing.T) {
	fd := newFakeDaemon(t)
	in := hookInput(t.TempDir(), "/reviewing-account-mr https://gitlab/x/-/merge_requests/1014 dont post")
	out, _ := runCLIStdin(t, addrOf(fd), in, "hook", "user-prompt")
	body := fd.body("/v1/retrieve")
	if body == nil || body["prompt"] != "reviewing-account-mr https://gitlab/x/-/merge_requests/1014 dont post" {
		t.Fatalf("a skill invocation is real work and must retrieve on its arguments, got %v", body)
	}
	if !strings.Contains(out, "imem-protocol") {
		t.Fatalf("the protocol must ride along, got %q", out)
	}
}

func TestHookPathPromptIsNotACommand(t *testing.T) {
	fd := newFakeDaemon(t)
	in := hookInput(t.TempDir(), "/Users/x/repo/usecase/guard.go why does this panic")
	runCLIStdin(t, addrOf(fd), in, "hook", "user-prompt")
	if body := fd.body("/v1/retrieve"); body == nil || body["prompt"] != "/Users/x/repo/usecase/guard.go why does this panic" {
		t.Fatalf("a prompt that starts with a path is an ordinary prompt, got %v", body)
	}
}

func TestHookBareCommandStillGetsTheProtocol(t *testing.T) {
	fd := newFakeDaemon(t)
	out, _ := runCLIStdin(t, addrOf(fd), hookInput(t.TempDir(), "/lanjut-review"), "hook", "user-prompt")
	if !strings.Contains(out, "imem-protocol") || fd.callCount("/v1/retrieve") != 1 {
		t.Fatalf("a bare command still gets the protocol and a context retrieve, got %q", out)
	}
}

func TestProtocolHasNoOptOut(t *testing.T) {
	if strings.Contains(selfSearchProtocol, "when the tool is available") {
		t.Fatal("the protocol must not offer a way out of searching")
	}
}

const searchLine = `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[` +
	`{"type":"tool_use","id":"t1","name":"mcp__imem__imem_search","input":{"query":"imem"}}]}}`

const uuidPrompt = `{"type":"user","uuid":"u1","message":{"role":"user","content":"fix the guard"}}`

func TestHookStopBlocksATurnThatNeverSearched(t *testing.T) {
	fd := newFakeDaemon(t)
	p := transcriptWith(t, uuidPrompt, replyLine)
	out, _ := runCLIStdin(t, addrOf(fd), stopInput(t, p, "Committed as bddae456.", false), "hook", "stop")
	if !strings.Contains(out, `"decision":"block"`) || !strings.Contains(out, "imem_search") {
		t.Fatalf("a turn without imem_search must be sent back to search, got %q", out)
	}
	if fd.callCount("/v1/flush") != 0 {
		t.Fatal("a blocked stop must not flush yet")
	}
	if b := fd.body("/v1/gate"); b["prompt_id"] != "u1" || b["searched"] != false {
		t.Fatalf("the gate needs the prompt and whether it searched, got %v", b)
	}
}

func TestHookStopPassesATurnThatSearched(t *testing.T) {
	fd := newFakeDaemon(t)
	p := transcriptWith(t, uuidPrompt, searchLine, replyLine)
	out, _ := runCLIStdin(t, addrOf(fd), stopInput(t, p, "Committed as bddae456.", false), "hook", "stop")
	if strings.Contains(out, `"decision"`) || fd.callCount("/v1/flush") != 1 {
		t.Fatalf("a turn that searched must stop normally and save, got %q", out)
	}
}

func TestHookStopGateCanBeTurnedOff(t *testing.T) {
	fd := newFakeDaemon(t)
	p := transcriptWith(t, uuidPrompt, replyLine)
	c := cliCall{addr: addrOf(fd), stdin: stopInput(t, p, "Committed as bddae456.", false), extra: `, "enforce_search": false`}
	out, _ := runCLIWith(t, c, "hook", "stop")
	if strings.Contains(out, `"decision"`) || fd.callCount("/v1/flush") != 1 || fd.callCount("/v1/gate") != 1 {
		t.Fatalf("with enforcement off the gate only counts, got %q", out)
	}
}

func TestHookStopContinuationRecordsTheSearch(t *testing.T) {
	fd := newFakeDaemon(t)
	p := transcriptWith(t, uuidPrompt, replyLine, searchLine, replyLine)
	runCLIStdin(t, addrOf(fd), stopInput(t, p, "Committed as bddae456.", true), "hook", "stop")
	if b := fd.body("/v1/gate"); b["searched"] != true {
		t.Fatalf("a search made after the gate fired must be counted, got %v", b)
	}
}
