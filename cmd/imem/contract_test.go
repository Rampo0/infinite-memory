package main

// Contract tests for the CLI surface that the ai-review and on-call agents
// (~/scratch/agentkit, ~/scratch/agents) depend on. They run the real binary
// entrypoint against a fake daemon and check stdout and exit code exactly as
// the agents see them:
//
//   - on-call preflight/doctor gate every full run on the FIRST LINE of
//     `imem status` containing "daemon: up" and "memgraph: true", exit 0;
//   - agentkit.imem.entity_matches parses `imem entities --limit 500` with
//     ENTITY_LINE below;
//   - `imem search "<q>" --cwd <workspace>` and `imem entity "<name>"` are
//     run by deterministic code and their stdout handed to a model.
//
// A failure here means a bot breaks silently — change the agents first.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/retrieve"
)

// agentEntityLine is agentkit/imem.py ENTITY_LINE, applied with re.match
// (anchored at the start of the line).
var agentEntityLine = regexp.MustCompile(`^\s*(\d+)x\s+(.+?)\s{2,}\[`)

func TestMain(m *testing.M) {
	// Re-exec hook: the contract tests run this test binary as `imem`.
	if os.Getenv("IMEM_CONTRACT_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeDaemon serves canned responses built from the daemon's real response
// types, so a JSON shape change on the daemon side also shows up here.
type fakeDaemon struct {
	*httptest.Server
	mu       sync.Mutex
	queries  map[string]url.Values
	calls    map[string]int
	lastBody map[string]any
	bodies   map[string]map[string]any
	at       map[string]time.Time
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	fd := &fakeDaemon{queries: map[string]url.Values{}, calls: map[string]int{}, bodies: map[string]map[string]any{},
		at: map[string]time.Time{}}
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		fd.mu.Lock()
		fd.queries[r.URL.Path] = r.URL.Query()
		fd.calls[r.URL.Path]++
		fd.at[r.URL.Path] = time.Now()
		fd.mu.Unlock()
	}
	keepBody := func(r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fd.mu.Lock()
		fd.bodies[r.URL.Path] = body
		fd.mu.Unlock()
	}
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"ok": true, "memgraph": true})
	})
	mux.HandleFunc("GET /v1/stats", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"memgraph": true, "projects": []graph.ProjectStats{
			{Key: "/Users/x/accountworkspace", Memories: 3, Entities: 5, Sessions: 1},
		}})
	})
	mux.HandleFunc("GET /v1/saved", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"sessions": []any{}})
	})
	mux.HandleFunc("GET /v1/backups", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"enabled": true, "interval_hours": 4, "keep": 2, "backups": []any{}})
	})
	mux.HandleFunc("GET /v1/entities", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, map[string]any{"project": "", "entities": []graph.EntityInfo{
			{Name: "hst_amend table", Etype: "table", ProjectKey: "/Users/x/accountworkspace/account", Mentions: 42},
			{Name: "bank verification flow", Etype: "flow", ProjectKey: "/Users/x/accountworkspace", Mentions: 10},
			{Name: "zero trust auth", Etype: "rfc", ProjectKey: "/Users/x/opening-account", Mentions: 3},
			// 40+ chars: %-40s pads nothing, so the separator alone must supply the
			// two spaces ENTITY_LINE needs (26 live entities were invisible to agents).
			{Name: "registration/repository/dttot/postgresql.go", Etype: "file", ProjectKey: "/x", Mentions: 1},
		}})
	})
	mux.HandleFunc("GET /v1/memories", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, map[string]any{"project": "/Users/x/accountworkspace", "memories": []retrieve.Scored{
			{Candidate: graph.Candidate{ID: "a1", Title: "Jago whitelist lives in master-data", Content: "Whitelist rows sit in master_data.jago_whitelist.", Kind: "fact"}, Score: 7.1},
			{Candidate: graph.Candidate{ID: "b2", Title: "Whitelist sync is daily", Content: "A cron syncs it at 02:00 WIB.", Kind: "decision"}, Score: 4.25},
		}})
	})
	mux.HandleFunc("GET /v1/entity", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, graph.EntityDetail{
			Query: r.URL.Query().Get("name"),
			Nodes: []graph.EntityInfo{{Name: "jago whitelist", Etype: "topic", ProjectKey: "/Users/x/accountworkspace", Mentions: 4}},
			Memories: []graph.EntityMemory{{Title: "Jago whitelist lives in master-data", Kind: "fact",
				Content: "Whitelist rows sit in master_data.jago_whitelist.", ProjectKey: "/Users/x/accountworkspace"}},
		})
	})
	mux.HandleFunc("GET /v1/rules", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, map[string]any{"project": "/Users/x/accountworkspace", "count": 2, "lines": []string{
			"- [rule] Functions stay under 60 lines — Split anything longer. (3d ago)",
			"- [rule] Never use --bare — It disables subscription OAuth. (1d ago, from infinite-memory)",
		}})
	})
	mux.HandleFunc("POST /v1/session-start", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fd.mu.Lock()
		fd.lastBody = body
		fd.mu.Unlock()
		reply(w, map[string]any{"context": "<infinite-memory project=\"/c\">\nStanding rules and conventions (follow these unless the user says otherwise):\n- [rule] Never use --bare — c (1d ago)\n</infinite-memory>",
			"rules": 1, "preferences": 0, "omitted": 0})
	})
	mux.HandleFunc("POST /v1/retrieve", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		keepBody(r)
		reply(w, map[string]any{"context": "", "memories": 0, "rules": 0})
	})
	mux.HandleFunc("POST /v1/flush", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		keepBody(r)
		reply(w, map[string]any{"ok": true, "saved": map[string]any{}})
	})
	mux.HandleFunc("POST /v1/extract", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		keepBody(r)
		w.WriteHeader(http.StatusAccepted)
		reply(w, map[string]any{"queued": true})
	})
	mux.HandleFunc("POST /v1/retrieve/preview", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"context": "<infinite-memory project=\"/c\">\n" +
			"Long-term memories from previous sessions (background knowledge; verify before relying on it):\n" +
			"- [fact] Jago Syariah binds to exactly one Stockbit account — c (1d ago)\n</infinite-memory>"})
	})
	fd.Server = httptest.NewServer(mux)
	t.Cleanup(fd.Close)
	return fd
}

func (fd *fakeDaemon) callCount(path string) int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.calls[path]
}

func (fd *fakeDaemon) calledAt(path string) time.Time {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.at[path]
}

func (fd *fakeDaemon) body(path string) map[string]any {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.bodies[path]
}

func (fd *fakeDaemon) query(path string) url.Values {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.queries[path]
}

// runCLI runs this test binary as `imem <args>` pointed at addr, returning
// stdout and the exit code.
func runCLI(t *testing.T, addr string, args ...string) (string, int) {
	t.Helper()
	return runCLIStdin(t, addr, "", args...)
}

// runCLIStdin is runCLI with stdin, for the hook entrypoints.
func runCLIStdin(t *testing.T, addr, stdin string, args ...string) (string, int) {
	t.Helper()
	return runCLIWith(t, cliCall{addr: addr, stdin: stdin}, args...)
}

type cliCall struct {
	addr, stdin string
	env         []string
}

var interactiveEnv = []string{"CLAUDE_CODE_SESSION_ATTENDED=1", "CLAUDE_CODE_ENTRYPOINT=cli", "IMEM_AGENT=", "CLAUDE_CODE_SESSION_ID="}

func runCLIWith(t *testing.T, c cliCall, args ...string) (string, int) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"http_addr": %q}`, c.addr)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "IMEM_CONTRACT_MAIN=1", "IMEM_CONFIG="+cfgPath)
	cmd.Env = append(append(cmd.Env, interactiveEnv...), c.env...)
	cmd.Stdin = strings.NewReader(c.stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return stdout.String(), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return stdout.String(), 0
}

func addrOf(fd *fakeDaemon) string { return strings.TrimPrefix(fd.URL, "http://") }

// on-call preflight.imem_status: returncode 0 and the first line carries both
// markers, otherwise the worker holds every full report.
func TestContractStatusFirstLine(t *testing.T) {
	out, code := runCLI(t, addrOf(newFakeDaemon(t)), "status")
	first, _, _ := strings.Cut(out, "\n")
	if code != 0 || !strings.Contains(first, "daemon: up") || !strings.Contains(first, "memgraph: true") {
		t.Fatalf("status contract broken: code=%d first line %q", code, first)
	}
}

// A down daemon must read as down: non-zero exit, no "daemon: up".
func TestContractStatusDown(t *testing.T) {
	out, code := runCLI(t, "127.0.0.1:1", "status")
	first, _, _ := strings.Cut(out, "\n")
	if code == 0 || strings.Contains(first, "daemon: up") {
		t.Fatalf("a dead daemon must not pass preflight: code=%d first line %q", code, first)
	}
}

// agentkit.imem.entities runs `imem entities --limit 500`; entity_matches
// keeps only lines matching ENTITY_LINE and reads count + name from them.
func TestContractEntitiesLines(t *testing.T) {
	fd := newFakeDaemon(t)
	out, code := runCLI(t, addrOf(fd), "entities", "--limit", "500")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := fd.query("/v1/entities").Get("limit"); got != "500" {
		t.Fatalf("--limit must reach the daemon, got %q", got)
	}
	want := map[string]string{
		"hst_amend table": "42", "bank verification flow": "10", "zero trust auth": "3",
		"registration/repository/dttot/postgresql.go": "1",
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if m := agentEntityLine.FindStringSubmatch(line); m != nil {
			got[m[2]] = m[1]
		}
	}
	for name, count := range want {
		if got[name] != count {
			t.Fatalf("entity %q: agent regex read count %q, want %q\noutput:\n%s", name, got[name], count, out)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("agent regex matched %d lines, want %d (a header line must never match)\noutput:\n%s", len(got), len(want), out)
	}
}

// ai-review run_imem / on-call prep: `imem search "<q>" --cwd <workspace>`,
// stdout saved verbatim for the model. Capped at 10 results by the CLI.
func TestContractSearchOutput(t *testing.T) {
	fd := newFakeDaemon(t)
	out, code := runCLI(t, addrOf(fd), "search", "jago whitelist", "--cwd", "/Users/x/accountworkspace")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	q := fd.query("/v1/memories")
	if q.Get("q") != "jago whitelist" || q.Get("cwd") != "/Users/x/accountworkspace" || q.Get("limit") != "10" {
		t.Fatalf("search must send q, cwd and limit=10, got %v", q)
	}
	want := "project: /Users/x/accountworkspace (2 hits)\n" +
		"  [fact] Jago whitelist lives in master-data — Whitelist rows sit in master_data.jago_whitelist. (score 7.10)\n" +
		"  [decision] Whitelist sync is daily — A cron syncs it at 02:00 WIB. (score 4.25)\n"
	if out != want {
		t.Fatalf("search output changed:\nwant:\n%s\ngot:\n%s", want, out)
	}
}

// `imem entity "<name>"`: the joined args are the entity name, and the
// first line names the entity before its per-project mention counts.
func TestContractEntityOutput(t *testing.T) {
	fd := newFakeDaemon(t)
	out, code := runCLI(t, addrOf(fd), "entity", "jago whitelist")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := fd.query("/v1/entity").Get("name"); got != "jago whitelist" {
		t.Fatalf("entity name must reach the daemon, got %q", got)
	}
	if !strings.HasPrefix(out, "jago whitelist [topic] — appears in: accountworkspace(4x)\n") {
		t.Fatalf("entity header changed:\n%s", out)
	}
	if !strings.Contains(out, "  [fact] Jago whitelist lives in master-data — Whitelist rows sit in master_data.jago_whitelist. (accountworkspace, ") {
		t.Fatalf("entity memory line changed:\n%s", out)
	}
}

// ai-review's prep.newest_rules() runs `imem rules --cwd ~/accountworkspace`
// and keeps the lines starting with "- [rule]" for its standing-rules file.
func TestContractRulesLines(t *testing.T) {
	fd := newFakeDaemon(t)
	out, code := runCLI(t, addrOf(fd), "rules", "--cwd", "/Users/x/accountworkspace")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := fd.query("/v1/rules").Get("cwd"); got != "/Users/x/accountworkspace" {
		t.Fatalf("--cwd must reach the daemon, got %q", got)
	}
	want := "- [rule] Functions stay under 60 lines — Split anything longer. (3d ago)\n" +
		"- [rule] Never use --bare — It disables subscription OAuth. (1d ago, from infinite-memory)\n"
	if out != want {
		t.Fatalf("rules output must be the bare lines:\nwant:\n%s\ngot:\n%s", want, out)
	}
}

// A down daemon must fail loudly (ai-review then falls back), never print an
// empty rules list that reads as "no rules".
func TestContractRulesDown(t *testing.T) {
	out, code := runCLI(t, "127.0.0.1:1", "rules")
	if code == 0 || out != "" {
		t.Fatalf("want non-zero exit and empty stdout, got code %d out %q", code, out)
	}
}
