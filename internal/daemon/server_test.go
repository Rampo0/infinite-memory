package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/retrieve"
)

// agentkitTranscript writes a transcript exactly as agentkit.imem.save does:
// one user line with a plain-string context, then the posted text as
// assistant text blocks.
func agentkitTranscript(t *testing.T, dir, name string) string {
	t.Helper()
	lines := []map[string]any{
		{"type": "user", "message": map[string]any{"role": "user", "content": "on-call report for trigger 21"}},
		{"type": "assistant", "message": map[string]any{"role": "assistant",
			"content": []map[string]any{{"type": "text", "text": "RDN creation stuck because the BCA callback was rejected."}}}},
	}
	var b bytes.Buffer
	for _, l := range lines {
		_ = json.NewEncoder(&b).Encode(l)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type jobRecorder struct {
	mu   sync.Mutex
	jobs []Job
}

func (r *jobRecorder) process(j Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs = append(r.jobs, j)
	return nil
}

func (r *jobRecorder) waitFor(t *testing.T, n int) []Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := append([]Job(nil), r.jobs...)
		r.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %d processed job(s) within 2s", n)
	return nil
}

func extractServer(t *testing.T, roots []string, rec *jobRecorder) *server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// An hour of debounce: only session_end's immediate enqueue can run the job.
	q := NewQueue(time.Hour, rec.process, log)
	q.Start()
	t.Cleanup(q.Stop)
	return &server{cfg: config.Config{AgentRoots: roots}, queue: q, saves: newSaveLog(), log: log}
}

func postExtract(s *server, body map[string]any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	s.handleExtract(w, httptest.NewRequest(http.MethodPost, "/v1/extract", bytes.NewReader(data)))
	return w
}

// Contract with agentkit.imem.save (ai-review, on-call): it posts a
// session_end extract for a transcript in its agent root and treats anything
// but 202 as a failed save. session_end must enqueue at once — the bot never
// sends a second event to fire a debounce.
func TestExtractAcceptsAgentTranscript(t *testing.T) {
	root := t.TempDir()
	path := agentkitTranscript(t, root, "on-call-21-9e38766e.jsonl")
	rec := &jobRecorder{}
	s := extractServer(t, []string{root}, rec)

	w := postExtract(s, map[string]any{
		"session_id": "on-call-21-9e38766e", "transcript_path": path,
		"cwd": "/Users/x/.on-call", "source": "session_end",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("agent save must get 202, got %d: %s", w.Code, w.Body.String())
	}
	jobs := rec.waitFor(t, 1)
	if jobs[0].TranscriptPath != path || jobs[0].CWD != "/Users/x/.on-call" {
		t.Fatalf("job must carry the agent's transcript and cwd, got %+v", jobs[0])
	}
}

func TestExtractRejectsTranscriptOutsideAgentRoots(t *testing.T) {
	path := agentkitTranscript(t, t.TempDir(), "stray.jsonl")
	s := extractServer(t, []string{t.TempDir()}, &jobRecorder{})
	w := postExtract(s, map[string]any{
		"session_id": "x", "transcript_path": path, "cwd": "/Users/x/.on-call", "source": "session_end",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a transcript outside every root must be refused, got %d", w.Code)
	}
}

func retrieveServer(t *testing.T) *server {
	t.Helper()
	// RulesK 0 and an all-stopword prompt keep Retrieve off the store, so
	// these tests need no Memgraph.
	return &server{cfg: config.Config{}, retr: &retrieve.Retriever{}, saves: newSaveLog(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func postRetrieve(s *server, preview bool, sid string) *httptest.ResponseRecorder {
	data, _ := json.Marshal(map[string]any{"cwd": "/c", "prompt": "the and", "session_id": sid})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(data))
	if preview {
		s.handleRetrievePreview(w, r)
	} else {
		s.handleRetrieve(w, r)
	}
	return w
}

// imem eval measures through the preview endpoint; it must never eat a save
// report the user's next prompt is waiting to print.
func TestRetrievePreviewLeavesSaveReports(t *testing.T) {
	s := retrieveServer(t)
	s.saves.Record(SaveReport{SessionID: "s1", At: time.Now().Unix(),
		Memories: []SavedMemory{{Title: "t", Kind: "fact", New: true, Seen: 1}}})

	if w := postRetrieve(s, true, "s1"); w.Code != http.StatusOK {
		t.Fatalf("preview: %d", w.Code)
	}
	if reports, _ := s.saves.Peek("s1"); len(reports) != 1 || reports[0].reported {
		t.Fatalf("preview drained the save log: %+v", reports)
	}
	postRetrieve(s, false, "s1")
	if reports, _ := s.saves.Peek("s1"); len(reports) != 1 || !reports[0].reported {
		t.Fatalf("the real retrieve should still drain: %+v", reports)
	}
}

func rulesServer(t *testing.T, cfg config.Config) *server {
	t.Helper()
	now := time.Now().Unix()
	cands := map[string][]graph.Candidate{
		"rule": {
			{ID: "foreign", Title: "Foreign rule", Kind: "rule", ProjectKey: "/other", Content: "x", SeenCount: 9, LastSeen: now},
			{ID: "local", Title: "Local rule", Kind: "rule", ProjectKey: "/here", Content: "Functions under 60 lines.", SeenCount: 1, LastSeen: now},
		},
		"preference": {
			{ID: "pref", Title: "Prefers stdlib", Kind: "preference", ProjectKey: "/here", Content: "No frameworks.", SeenCount: 1, LastSeen: now},
		},
	}
	s := retrieveServer(t)
	s.cfg = cfg
	s.injections = newInjectionLog()
	s.byKind = func(_ context.Context, kind string) ([]graph.Candidate, error) { return cands[kind], nil }
	return s
}

func sessionStart(s *server, sid, source string) map[string]any {
	data, _ := json.Marshal(map[string]any{"session_id": sid, "cwd": "/here", "source": source})
	w := httptest.NewRecorder()
	s.handleSessionStart(w, httptest.NewRequest(http.MethodPost, "/v1/session-start", bytes.NewReader(data)))
	var out map[string]any
	_ = json.NewDecoder(w.Body).Decode(&out)
	return out
}

func TestSessionStartInjectsRepoRulesAndPreferences(t *testing.T) {
	s := rulesServer(t, config.Config{RulesOnSessionStart: true, RulesMaxChars: 8000, MaxMemoryContentChars: 400})
	out := sessionStart(s, "s1", "startup")
	block, _ := out["context"].(string)
	local := strings.Index(block, "- [rule] Local rule")
	pref := strings.Index(block, "- [preference] Prefers stdlib")
	foreign := strings.Index(block, "- [rule] Foreign rule")
	if local < 0 || pref < 0 || foreign < 0 || !(local < pref && pref < foreign) {
		t.Fatalf("want this repo's rule, then preferences, then other repos' rules:\n%s", block)
	}
}

// After /clear or a compaction, earlier injections are gone from the model's
// context, so they may be injected again; a resume keeps them.
func TestSessionStartResetsInjectionsOnClearOrCompact(t *testing.T) {
	s := rulesServer(t, config.Config{RulesOnSessionStart: true, RulesMaxChars: 8000})
	for source, keep := range map[string]bool{"resume": true, "subagent": true, "compact": false, "clear": false, "startup": false} {
		s.injections.Record("s1", []injected{{ID: "m1"}})
		sessionStart(s, "s1", source)
		if got := s.injections.Seen("s1")["m1"]; got != keep {
			t.Fatalf("source %s: injections kept = %v, want %v", source, got, keep)
		}
		s.injections.Reset("s1")
	}
}

func TestSessionStartSilentWhenRulesStayPerPrompt(t *testing.T) {
	s := rulesServer(t, config.Config{RulesOnSessionStart: false, RulesMaxChars: 8000})
	if block, _ := sessionStart(s, "s1", "startup")["context"].(string); block != "" {
		t.Fatalf("rules_on_session_start=false keeps rules per prompt, got:\n%s", block)
	}
}

// GET /v1/rules backs `imem rules`, which ai-review reads instead of scraping
// oversized hook output: every live rule, this project first, one per line.
func TestRulesEndpointLines(t *testing.T) {
	s := rulesServer(t, config.Config{MaxMemoryContentChars: 400})
	w := httptest.NewRecorder()
	s.handleRules(w, httptest.NewRequest(http.MethodGet, "/v1/rules?cwd=/here", nil))
	var out struct {
		Lines []string `json:"lines"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Lines) != 2 || !strings.HasPrefix(out.Lines[0], "- [rule] Local rule — ") ||
		!strings.HasPrefix(out.Lines[1], "- [rule] Foreign rule — x (") {
		t.Fatalf("want both rules, this project first, as block lines: %q", out.Lines)
	}
}

func pinFirstRule(s *server) {
	base := s.byKind
	s.byKind = func(ctx context.Context, kind string) ([]graph.Candidate, error) {
		cs, err := base(ctx, kind)
		out := append([]graph.Candidate(nil), cs...)
		if kind == "rule" {
			out[0].Pinned = true
		}
		return out, err
	}
}

func TestRulesEndpointPinnedOnly(t *testing.T) {
	s := rulesServer(t, config.Config{MaxMemoryContentChars: 400})
	pinFirstRule(s)
	w := httptest.NewRecorder()
	s.handleRules(w, httptest.NewRequest(http.MethodGet, "/v1/rules?cwd=/here&pinned=1", nil))
	var out struct {
		Lines []string `json:"lines"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Lines) != 1 || !strings.HasPrefix(out.Lines[0], "- [rule] Foreign rule") {
		t.Fatalf("pinned=1 must list only pinned rules: %q", out.Lines)
	}
}

func TestSessionStartPutsPinnedFirst(t *testing.T) {
	s := rulesServer(t, config.Config{RulesOnSessionStart: true, RulesMaxChars: 8000, MaxMemoryContentChars: 400})
	pinFirstRule(s)
	block, _ := sessionStart(s, "s1", "startup")["context"].(string)
	pinned := strings.Index(block, "- [rule] Foreign rule")
	local := strings.Index(block, "- [rule] Local rule")
	if pinned < 0 || local < 0 || pinned > local {
		t.Fatalf("a pinned rule from another repo must come before this repo's rules:\n%s", block)
	}
}

type savedCall struct {
	pk, sid string
	mems    []graph.MemoryIn
}

func rememberServer(t *testing.T, got *savedCall) *server {
	t.Helper()
	s := retrieveServer(t)
	s.saveBatch = func(_ context.Context, pk, sid string, _ int64, mems []graph.MemoryIn) ([]graph.SaveOutcome, error) {
		*got = savedCall{pk, sid, mems}
		return []graph.SaveOutcome{{ID: "abc", Title: mems[0].Title, Kind: mems[0].Kind, New: true, Seen: 1}}, nil
	}
	return s
}

func postRemember(s *server, body map[string]any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	s.handleRemember(w, httptest.NewRequest(http.MethodPost, "/v1/remember", bytes.NewReader(data)))
	return w
}

// imem_remember (MCP) saves one explicit memory under the session's project.
func TestRememberSavesExplicitMemory(t *testing.T) {
	var got savedCall
	s := rememberServer(t, &got)
	cwd := t.TempDir()
	w := postRemember(s, map[string]any{"cwd": cwd, "title": "Never use --bare", "content": "It disables OAuth.",
		"kind": "rule", "entities": []string{"claude cli", "  ", "oauth"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got.pk != cwd || !strings.HasPrefix(got.sid, "mcp-") || len(got.mems) != 1 {
		t.Fatalf("want one memory under the cwd's project in an mcp- session, got %+v", got)
	}
	m := got.mems[0]
	if m.Kind != "rule" || m.Title != "Never use --bare" || len(m.Entities) != 2 || m.Entities[0].Name != "claude cli" {
		t.Fatalf("memory not passed through: %+v", m)
	}
	if !strings.Contains(w.Body.String(), `"new":true`) {
		t.Fatalf("the reply must say whether it was new: %s", w.Body.String())
	}
}

func TestRememberRejectsInvalid(t *testing.T) {
	var got savedCall
	s := rememberServer(t, &got)
	for _, body := range []map[string]any{
		{"cwd": "/c", "title": "t", "content": "c", "kind": "gossip"},
		{"cwd": "/c", "title": " ", "content": "c", "kind": "fact"},
		{"cwd": "/c", "title": "t", "content": "", "kind": "fact"},
	} {
		if w := postRemember(s, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%v: want 400, got %d", body, w.Code)
		}
	}
	if got.mems != nil {
		t.Fatal("nothing may be saved from an invalid request")
	}
}

func TestRememberClipsContent(t *testing.T) {
	var got savedCall
	s := rememberServer(t, &got)
	postRemember(s, map[string]any{"cwd": "/c", "title": "t", "content": strings.Repeat("x", 4000), "kind": "fact"})
	if len(got.mems[0].Content) != 1500 {
		t.Fatalf("content must be clipped to 1500, got %d", len(got.mems[0].Content))
	}
}

func TestContextPlanUsesTheConversationForWeakPrompts(t *testing.T) {
	ctx := "verify the imem retrieval spec\nhooks inject memories per prompt"
	cases := []struct {
		prompt          string
		first, fallback bool
	}{
		{"lanjut", true, false},
		{"fix that", true, false},
		{"jago syariah connect akun stockbit", false, true},
	}
	for _, c := range cases {
		first, fallback := contextPlan(retrieveReq{Prompt: c.prompt, Context: ctx})
		if (len(first) > 0) != c.first || (len(fallback) > 0) != c.fallback {
			t.Fatalf("%q: first=%v fallback=%v", c.prompt, first, fallback)
		}
	}
	if first, fallback := contextPlan(retrieveReq{Prompt: "lanjut"}); first != nil || fallback != nil {
		t.Fatal("no context, no extra terms")
	}
}

func TestMergeTermsAppendsAfterThePromptTokens(t *testing.T) {
	if mergeTerms(nil, nil) != nil {
		t.Fatal("nothing to add must leave Expand nil")
	}
	got := mergeTerms([]string{"llm"}, []string{"imem"})([]string{"lanjut"})
	if strings.Join(got, ",") != "lanjut,llm,imem" {
		t.Fatalf("got %v", got)
	}
}

func TestExtractCarriesTheHeadlessFlag(t *testing.T) {
	root := t.TempDir()
	path := agentkitTranscript(t, root, "headless.jsonl")
	rec := &jobRecorder{}
	s := extractServer(t, []string{root}, rec)
	postExtract(s, map[string]any{"session_id": "h1", "transcript_path": path, "cwd": "/c", "source": "session_end", "agent": true})
	if jobs := rec.waitFor(t, 1); !jobs[0].Agent {
		t.Fatalf("an agent extract must reach the worker flagged, got %+v", jobs[0])
	}
}

func TestSessionStartWritesTheFetchInstructionAndPinnedItemsToTheRulesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imem-rules.md")
	s := rulesServer(t, config.Config{RulesOnSessionStart: true, RulesMaxChars: 8000, MaxMemoryContentChars: 400, RulesFile: path})
	pinFirstRule(s)
	out := sessionStart(s, "s1", "startup")
	if out["rules_file"] != float64(2) || out["preferences_file"] != float64(1) || out["pinned"] != float64(1) {
		t.Fatalf("the response must count live rules, preferences and pinned items: %v", out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), " rules --here`") || !strings.Contains(string(data), "- [rule] Foreign rule — x (from other)") {
		t.Fatalf("rules file needs the fetch command and the pinned rule:\n%s", data)
	}
	if strings.Contains(string(data), "Local rule") || strings.Contains(string(data), "Prefers stdlib") {
		t.Fatalf("unpinned items are fetched on demand, not written:\n%s", data)
	}
}

func TestRulesEndpointHereListsThisProjectInFull(t *testing.T) {
	s := rulesServer(t, config.Config{MaxMemoryContentChars: 5})
	w := httptest.NewRecorder()
	s.handleRules(w, httptest.NewRequest(http.MethodGet, "/v1/rules?cwd=/here&here=1", nil))
	var out struct {
		Lines []string `json:"lines"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"- [rule] Local rule — Functions under 60 lines. (from here)",
		"- [preference] Prefers stdlib — No frameworks. (from here)",
		"… 1 rules + 0 preferences from other projects not shown (imem rules lists every rule)",
	}
	if strings.Join(out.Lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("want this project's rules and preferences unclipped plus a footer:\n%q", out.Lines)
	}
}

func TestWriteFileAtomicReplacesWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.md")
	for _, text := range []string{"first version, longer", "second"} {
		if err := writeFileAtomic(path, text); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != "second" {
		t.Fatalf("got %q", data)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestExtractSessionEndIsAFinalJob(t *testing.T) {
	root := t.TempDir()
	path := agentkitTranscript(t, root, "end.jsonl")
	rec := &jobRecorder{}
	s := extractServer(t, []string{root}, rec)
	postExtract(s, map[string]any{"session_id": "e1", "transcript_path": path, "cwd": "/c", "source": "session_end"})
	if jobs := rec.waitFor(t, 1); !jobs[0].Final {
		t.Fatalf("a session end must extract even a short closing exchange, got %+v", jobs[0])
	}
}

func TestExtractRemembersWhereTheTranscriptLives(t *testing.T) {
	root := t.TempDir()
	path := agentkitTranscript(t, root, "bot.jsonl")
	rec := &jobRecorder{}
	s := extractServer(t, []string{root}, rec)
	var touched []graph.SessionSource
	s.touch = func(_ context.Context, src graph.SessionSource, _ int64) error {
		touched = append(touched, src)
		return nil
	}
	postExtract(s, map[string]any{"session_id": "b1", "transcript_path": path, "cwd": "/c", "source": "session_end", "agent": true})
	if len(touched) != 1 || touched[0].TranscriptPath != path || !touched[0].Agent || touched[0].ID != "b1" {
		t.Fatalf("the sweep needs the transcript path and agent flag, got %+v", touched)
	}
}

func TestSessionStartLeavesPinnedItemsToTheRulesFileExceptForSubagents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imem-rules.md")
	s := rulesServer(t, config.Config{RulesOnSessionStart: true, RulesMaxChars: 8000, MaxMemoryContentChars: 400, RulesFile: path})
	pinFirstRule(s)
	if block, _ := sessionStart(s, "s1", "startup")["context"].(string); block != "" {
		t.Fatalf("the session already imports the rules file, nothing rides in the block:\n%s", block)
	}
	block, _ := sessionStart(s, "s1", "subagent")["context"].(string)
	if !strings.Contains(block, "- [rule] Foreign rule") || strings.Contains(block, "Local rule") {
		t.Fatalf("a subagent gets the pinned items only:\n%s", block)
	}
}

func TestExtractAcceptsAnAgentRegisteredAfterStart(t *testing.T) {
	agentsDir := t.TempDir()
	root := t.TempDir()
	path := agentkitTranscript(t, root, "late.jsonl")
	rec := &jobRecorder{}
	s := extractServer(t, nil, rec)
	s.roots = newRootSet(nil, agentsDir)
	if w := postExtract(s, map[string]any{"session_id": "l1", "transcript_path": path, "cwd": "/c", "source": "session_end"}); w.Code != http.StatusBadRequest {
		t.Fatalf("an unregistered root must be refused, got %d", w.Code)
	}
	drop := fmt.Sprintf(`{"name": "late-bot", "root": %q}`, root)
	if err := os.WriteFile(filepath.Join(agentsDir, "late-bot.json"), []byte(drop), 0o644); err != nil {
		t.Fatal(err)
	}
	s.roots.checked = time.Time{}
	if w := postExtract(s, map[string]any{"session_id": "l1", "transcript_path": path, "cwd": "/c", "source": "session_end"}); w.Code != http.StatusAccepted {
		t.Fatalf("a drop-in written after start must be honoured without a restart, got %d", w.Code)
	}
}
