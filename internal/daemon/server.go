package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Rampo0/infinite-memory/internal/backup"
	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/expand"
	"github.com/Rampo0/infinite-memory/internal/extract"
	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/project"
	"github.com/Rampo0/infinite-memory/internal/retrieve"
	"github.com/Rampo0/infinite-memory/internal/textutil"
)

type server struct {
	cfg   config.Config
	store *graph.Store
	retr  *retrieve.Retriever
	queue *Queue
	saves *saveLog
	log   *slog.Logger

	// expander is nil-safe and shared: its vocabulary cache is per-process
	// state that must not be duplicated per request.
	expander *expand.Expander
	vocab    *vocabCache
	corpus   *corpusCache
	// injections tracks what each session was already shown.
	injections *injectionLog
	gates      *gateLog
	// byKind fetches every live memory of one kind (func field so handlers
	// test without Memgraph, the same injection shape as Expander.Run).
	byKind func(ctx context.Context, kind string) ([]graph.Candidate, error)
	// saveBatch writes memories (Store.SaveBatch; a func field for tests).
	saveBatch func(ctx context.Context, pk, sid string, now int64, mems []graph.MemoryIn) ([]graph.SaveOutcome, error)
	touch     func(ctx context.Context, src graph.SessionSource, now int64) error

	schemaMu sync.Mutex
	schemaOK bool

	// backupMu serialises dump runs; backupStateMu guards the reported state
	// so /v1/backups never blocks behind a running dump.
	backupMu      sync.Mutex
	backupStateMu sync.Mutex
	lastBackup    backup.Info
	lastBackupErr string
}

// Run starts the daemon in the foreground. The port bind doubles as the
// singleton lock: a second instance fails to bind and exits.
func Run(cfg config.Config) error {
	log := openLog(cfg)
	store, err := graph.New(cfg.MemgraphURI, cfg.MemgraphUser, cfg.MemgraphPass)
	if err != nil {
		return err
	}
	s := newServer(cfg, store, log)
	worker := s.newWorker()
	s.expander = s.newExpander(worker.Runner)
	s.queue = NewQueue(cfg.Debounce(), worker.Process, log)
	worker.Requeue = s.queue.enqueue
	s.queue.OnPending = s.saves.MarkPending
	s.queue.Start()
	s.startLoops(worker.Runner)
	// Memgraph may be down at startup: log and keep serving, the schema is
	// re-attempted on the next successful health check.
	s.ensureSchema(context.Background())
	go s.refreshRulesFile()
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("bind %s failed (daemon already running?): %w", cfg.HTTPAddr, err)
	}
	log.Info("imem daemon listening", "addr", cfg.HTTPAddr, "memgraph", cfg.MemgraphURI)
	return http.Serve(ln, s.routes())
}

func openLog(cfg config.Config) *slog.Logger {
	logPath := cfg.LogPath()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	var w io.Writer = os.Stderr
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		w = io.MultiWriter(os.Stderr, f)
	}
	return slog.New(slog.NewTextHandler(w, nil))
}

func newServer(cfg config.Config, store *graph.Store, log *slog.Logger) *server {
	corpus := newCorpusCache(store.CountLive)
	s := &server{
		cfg:   cfg,
		store: store,
		retr: &retrieve.Retriever{
			Store: store, K: cfg.RetrieveK, MaxContentChars: cfg.MaxMemoryContentChars,
			SameProjectBoost: cfg.SameProjectBoost, RulesK: cfg.RulesK,
			SummaryLines: cfg.HookSummaryLines,
			Corpus:       corpus.Get, MinMatch: cfg.MinMatch, RelatedMinWeight: cfg.RelatedMinWeight,
			MaxChars: cfg.RetrieveMaxChars,
		},
		corpus:     corpus,
		injections: loadInjectionLog(cfg),
		gates:      newGateLog(),
		byKind: func(ctx context.Context, kind string) ([]graph.Candidate, error) {
			return store.ByKind(ctx, kind, -1)
		},
		saveBatch: store.SaveBatch,
		touch:     store.TouchSession,
		saves:     newSaveLog(),
		vocab:     newVocabCache(),
		log:       log,
	}
	if cfg.RulesOnSessionStart {
		// Standing rules arrive once per session; per prompt only matches.
		s.retr.RulesK = 0
	}
	return s
}

func (s *server) newWorker() *Worker {
	worker := NewWorker(s.store, s.cfg, s.log)
	worker.Saves = s.saves
	// B1: the extractor sees the 12 existing memories most like the excerpt.
	similar := retrieve.Retriever{Store: s.store, K: 12, Corpus: s.corpus.Get,
		MinMatch: s.cfg.MinMatch, RelatedMinWeight: s.cfg.RelatedMinWeight}
	worker.Similar = func(ctx context.Context, pk string, tokens []string) ([]extract.Known, error) {
		scored, err := similar.QueryTokens(ctx, pk, tokens, time.Now().Unix())
		out := make([]extract.Known, len(scored))
		for i, m := range scored {
			out[i] = extract.Known{ID: m.ID, Kind: m.Kind, Title: m.Title, Content: m.Content}
		}
		return out, err
	}
	// B2: and the memories this session was shown since its last extraction.
	worker.Shown = func(sid string, since int64) []extract.Known {
		var out []extract.Known
		for _, it := range s.injections.Since(sid, since) {
			out = append(out, extract.Known{ID: it.ID, Kind: it.Kind, Title: it.Title, Content: it.Content})
		}
		return out
	}
	return worker
}

func (s *server) startLoops(runner *extract.Runner) {
	s.startSweepLoop()
	if s.cfg.BackupEnabled {
		s.startBackupLoop()
	}
	if s.cfg.ConsolidateEnabled {
		s.startConsolidateLoop(runner)
	}
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/retrieve", s.handleRetrieve)
	mux.HandleFunc("POST /v1/retrieve/preview", s.handleRetrievePreview)
	mux.HandleFunc("POST /v1/session-start", s.handleSessionStart)
	mux.HandleFunc("GET /v1/rules", s.handleRules)
	mux.HandleFunc("POST /v1/remember", s.handleRemember)
	mux.HandleFunc("POST /v1/extract", s.handleExtract)
	mux.HandleFunc("GET /v1/memories", s.handleMemories)
	mux.HandleFunc("GET /v1/expand", s.handleExpand)
	mux.HandleFunc("GET /v1/entities", s.handleEntities)
	mux.HandleFunc("GET /v1/entity", s.handleEntity)
	mux.HandleFunc("GET /v1/stats", s.handleStats)
	mux.HandleFunc("GET /v1/saved", s.handleSaved)
	mux.HandleFunc("POST /v1/flush", s.handleFlush)
	mux.HandleFunc("POST /v1/gate", s.handleGate)
	mux.HandleFunc("GET /v1/gate", s.handleGateStats)
	mux.HandleFunc("POST /v1/backup", s.handleBackup)
	mux.HandleFunc("GET /v1/backups", s.handleBackups)
	return mux
}

func (s *server) ensureSchema(ctx context.Context) {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if s.schemaOK {
		return
	}
	if err := s.store.EnsureSchema(ctx); err != nil {
		s.log.Warn("schema init failed (memgraph down?)", "err", err)
		return
	}
	s.schemaOK = true
	s.log.Info("schema ensured")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
	defer cancel()
	mgUp := s.store.Ping(ctx) == nil
	if mgUp {
		s.ensureSchema(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "memgraph": mgUp})
}

type retrieveReq struct {
	CWD       string `json:"cwd"`
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"`
	Context   string `json:"context"`
}

// handleRetrieve is the UserPromptSubmit path. It always answers 200:
// failures fail open into empty context.
func (s *server) handleRetrieve(w http.ResponseWriter, r *http.Request) {
	s.serveRetrieve(w, r, true)
}

// handleRetrievePreview returns exactly what handleRetrieve would inject, with
// no side effects: imem eval measures through it, and must never drain a save
// report a real prompt is waiting to print.
func (s *server) handleRetrievePreview(w http.ResponseWriter, r *http.Request) {
	s.serveRetrieve(w, r, false)
}

// serveRetrieve is the shared retrieve path; live marks a real prompt, the
// only kind allowed to touch per-session state.
func (s *server) serveRetrieve(w http.ResponseWriter, r *http.Request, live bool) {
	empty := map[string]any{"context": "", "count": 0, "summary": "", "memories": 0, "rules": 0}
	var req retrieveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	// Drained once, up front, and attached to EVERY response below: a
	// Memgraph-down retrieve must not swallow the user's save report.
	var saved SavedPayload
	if live {
		saved = s.saves.Drain(req.SessionID, s.cfg.HookSavedLines)
	}
	empty["saved"] = saved
	pk := project.ResolveKey(req.CWD)
	start := time.Now()

	// Expansion spends its own budget first; the graph deadline below is
	// untouched, so a slow or failed spawn costs time but never correctness.
	exp := s.expandFor(r.Context(), pk, req.Prompt)

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RetrieveTO())
	defer cancel()
	retr := *s.retr
	retr.Exclude = s.injections.Seen(req.SessionID)
	res, ctxTerms, err := retrieveFor(ctx, retr, req, exp.Added)
	if err != nil {
		s.log.Warn("retrieve failed", "err", err, "project", pk)
		writeJSON(w, http.StatusOK, empty)
		return
	}
	if live {
		s.injections.Record(req.SessionID, toInjected(res.Injected))
		s.markInjected(res.Injected)
	}
	s.log.Info("retrieve", "project", pk, "count", res.Memories+res.Rules,
		"memories", res.Memories, "rules", res.Rules, "omitted", res.Omitted,
		"already_shown", len(retr.Exclude), "chars", len(res.Block), "live", live,
		"expanded", len(exp.Added), "ctx_terms", ctxTerms, "llm_ms", exp.MS, "ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, map[string]any{
		"context": res.Block, "count": res.Memories + res.Rules,
		"summary": res.Summary, "memories": res.Memories, "rules": res.Rules,
		"saved": saved, "expanded": expandedLine(exp),
	})
}

const (
	weakPromptTokens = 3
	contextTermCap   = 16
)

func retrieveFor(ctx context.Context, retr retrieve.Retriever, req retrieveReq, base []string) (retrieve.Result, int, error) {
	pk := project.ResolveKey(req.CWD)
	now := time.Now().Unix()
	first, fallback := contextPlan(req)
	retr.Expand = mergeTerms(base, first)
	res, err := retr.Retrieve(ctx, pk, req.Prompt, now)
	if err != nil || res.Memories > 0 || len(fallback) == 0 {
		return res, len(first), err
	}
	retr.Expand = mergeTerms(base, fallback)
	if second, err := retr.Retrieve(ctx, pk, req.Prompt, now); err == nil && second.Memories > 0 {
		return second, len(fallback), nil
	}
	return res, len(first), nil
}

func contextPlan(req retrieveReq) (first, fallback []string) {
	terms := textutil.Tokenize(req.Context, contextTermCap)
	if len(terms) == 0 {
		return nil, nil
	}
	if len(textutil.Tokenize(req.Prompt, retrieve.PromptTokenCap)) < weakPromptTokens {
		return terms, nil
	}
	return nil, terms
}

func mergeTerms(base, terms []string) func([]string) []string {
	added := append(append([]string(nil), base...), terms...)
	if len(added) == 0 {
		return nil
	}
	return func(tokens []string) []string { return expand.Merge(tokens, added) }
}

type sessionStartReq struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	Source    string `json:"source"`
}

// handleSessionStart serves the SessionStart hook: it forgets what the session
// was shown when that context is gone (startup, /clear, compaction — not a
// resume, and not a subagent, whose hook carries its parent's session id),
// then returns the standing context: pinned rules, this project's rules and
// the user's preferences, within rules_max_chars. Always 200, failing open.
func (s *server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	empty := map[string]any{"context": "", "rules": 0, "preferences": 0, "omitted": 0}
	var req sessionStartReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	if req.Source != "resume" && req.Source != "subagent" {
		s.injections.Reset(req.SessionID)
	}
	if !s.cfg.RulesOnSessionStart {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	rules, err := s.byKind(ctx, "rule")
	if err != nil {
		s.log.Warn("session-start rules failed", "err", err)
		writeJSON(w, http.StatusOK, empty)
		return
	}
	prefs, err := s.byKind(ctx, "preference")
	if err != nil {
		s.log.Warn("session-start preferences failed", "err", err)
	}
	pk := project.ResolveKey(req.CWD)
	out := s.standingBlock(pk, rules, prefs)
	s.log.Info("session-start", "project", pk, "source", req.Source, "rules", out["rules"],
		"preferences", out["preferences"], "omitted", out["omitted"], "chars", len(out["context"].(string)))
	writeJSON(w, http.StatusOK, out)
}

func (s *server) standingBlock(pk string, rules, prefs []graph.Candidate) map[string]any {
	standing := retrieve.SessionRules(rules, prefs, pk)
	out := map[string]any{"rules_file": 0, "preferences_file": 0}
	if s.cfg.RulesFile != "" {
		s.writeRulesFile(rules, prefs)
		out["rules_file"], out["preferences_file"] = len(rules), len(prefs)
		standing = pinnedOnly(standing)
	} else if s.cfg.RulesK > 0 && len(standing) > s.cfg.RulesK {
		standing = standing[:s.cfg.RulesK]
	}
	block, _, shown := "", []retrieve.Scored(nil), []graph.Candidate(nil)
	if len(standing) > 0 {
		block, _, shown = retrieve.BuildBlock(retrieve.BlockInput{PK: pk, Rules: standing,
			MaxContentChars: s.cfg.MaxMemoryContentChars, MaxChars: s.cfg.RulesMaxChars, Now: time.Now().Unix()})
	}
	nRules := 0
	for _, c := range shown {
		if c.Kind == "rule" {
			nRules++
		}
	}
	out["context"], out["rules"], out["preferences"] = block, nRules, len(shown)-nRules
	out["omitted"] = len(standing) - len(shown)
	return out
}

func (s *server) writeRulesFile(rules, prefs []graph.Candidate) {
	text := retrieve.RulesFileText(rules, prefs)
	if old, err := os.ReadFile(s.cfg.RulesFile); err == nil && string(old) == text {
		return
	}
	if err := writeFileAtomic(s.cfg.RulesFile, text); err != nil {
		s.log.Warn("rules file not written", "path", s.cfg.RulesFile, "err", err)
		return
	}
	s.log.Info("rules file written", "path", s.cfg.RulesFile, "rules", len(rules), "preferences", len(prefs), "chars", len(text))
}

func writeFileAtomic(path, text string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".imem-rules-*")
	if err != nil {
		return err
	}
	_, werr := f.WriteString(text)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(f.Name())
		return fmt.Errorf("write %s: %v %v", f.Name(), werr, cerr)
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

func (s *server) refreshRulesFile() {
	if s.cfg.RulesFile == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rules, err := s.byKind(ctx, "rule")
	if err != nil {
		s.log.Warn("rules file refresh failed", "err", err)
		return
	}
	prefs, err := s.byKind(ctx, "preference")
	if err != nil {
		s.log.Warn("rules file refresh failed", "err", err)
		return
	}
	s.writeRulesFile(rules, prefs)
}

// handleRules lists every live rule, this project's first, one line each in
// the context-block format. `imem rules` prints it; ai-review's prep reads it
// for its standing-rules file.
func (s *server) handleRules(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	pk := project.ResolveKey(qv.Get("cwd"))
	limit := 0
	if n, err := strconv.Atoi(qv.Get("limit")); err == nil && n > 0 {
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rules, err := s.byKind(ctx, "rule")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if qv.Get("pinned") == "1" {
		rules = pinnedOnly(rules)
	}
	sorted := retrieve.SortRules(rules, pk, limit, nil)
	lines := retrieve.RuleLines(pk, sorted, s.cfg.MaxMemoryContentChars, time.Now().Unix())
	writeJSON(w, http.StatusOK, map[string]any{"project": pk, "lines": lines, "count": len(lines)})
}

func pinnedOnly(cs []graph.Candidate) []graph.Candidate {
	var out []graph.Candidate
	for _, c := range cs {
		if c.Pinned {
			out = append(out, c)
		}
	}
	return out
}

type rememberReq struct {
	CWD      string   `json:"cwd"`
	Title    string   `json:"title"`
	Content  string   `json:"content"`
	Kind     string   `json:"kind"`
	Entities []string `json:"entities"`
}

var rememberKinds = map[string]bool{"fact": true, "decision": true, "preference": true, "rule": true, "reference": true}

// handleRemember saves one explicit memory from the imem_remember MCP tool,
// under the caller's project, in a per-day "mcp-" session. It goes through
// the same SaveBatch as extraction: dedup and same-title supersede apply.
func (s *server) handleRemember(w http.ResponseWriter, r *http.Request) {
	var req rememberReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	title, content := strings.TrimSpace(req.Title), strings.TrimSpace(req.Content)
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if title == "" || content == "" || !rememberKinds[kind] {
		http.Error(w, "title, content and a valid kind are required", http.StatusBadRequest)
		return
	}
	if len(content) > 1500 {
		content = content[:1500]
	}
	mem := graph.MemoryIn{Title: title, Content: content, Kind: kind}
	for _, e := range req.Entities {
		if e = strings.TrimSpace(e); e != "" && len(mem.Entities) < 6 {
			mem.Entities = append(mem.Entities, graph.EntityIn{Name: e, Type: "topic"})
		}
	}
	pk := project.ResolveKey(req.CWD)
	now := time.Now()
	sid := "mcp-" + now.Format("20060102")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	saved, err := s.saveBatch(ctx, pk, sid, now.Unix(), []graph.MemoryIn{mem})
	if err != nil || len(saved) == 0 {
		s.log.Warn("remember failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprint(err)})
		return
	}
	s.log.Info("remembered", "project", pk, "kind", kind, "title", title, "new", saved[0].New)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": saved[0].ID, "title": saved[0].Title, "kind": saved[0].Kind,
		"new": saved[0].New, "seen": saved[0].Seen, "project": pk,
	})
}

// markInjected counts the injection on the memories, off the prompt's path.
func (s *server) markInjected(mems []retrieve.Scored) {
	if s.store == nil || len(mems) == 0 {
		return
	}
	ids := make([]string, len(mems))
	for i, m := range mems {
		ids[i] = m.ID
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.MarkInjected(ctx, ids, time.Now().Unix()); err != nil {
			s.log.Warn("mark injected failed", "err", err)
		}
	}()
}

func toInjected(mems []retrieve.Scored) []injected {
	out := make([]injected, len(mems))
	for i, m := range mems {
		out[i] = injected{ID: m.ID, Title: m.Title, Content: m.Content, Kind: m.Kind}
	}
	return out
}

func toSearched(mems []retrieve.Scored, query string) []injected {
	out := toInjected(mems)
	for i := range out {
		out[i].Via, out[i].Query = viaSearch, query
	}
	return out
}

// expandedLine is the pre-rendered user-facing summary of one expansion, so
// the hook binary stays dumb (same pattern as Result.Summary). Empty when
// expansion did not run or added nothing.
func expandedLine(exp expand.Result) string {
	if exp.TimedOut() {
		// Never let a timeout read as "the model had nothing to add": one
		// means raise the budget, the other means the prompt was fine.
		return fmt.Sprintf("expansion timed out (%dms)", exp.MS)
	}
	if exp.Err != nil {
		return "expansion failed"
	}
	if len(exp.Added) == 0 {
		return ""
	}
	line := fmt.Sprintf("+%d expanded", len(exp.Added))
	if exp.Intent != "" {
		line += "\n  ↳ " + exp.Intent
	}
	return line
}

// handleExpand shows what the expander would add, without searching. This is
// the only way to judge an expansion before it starts shaping context.
func (s *server) handleExpand(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	pk := project.ResolveKey(qv.Get("cwd"))
	prompt := qv.Get("q")
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ExpandBudget()+time.Second)
	defer cancel()
	// Deliberately bypasses cfg.ExpandEnabled: the point of the command is to
	// evaluate expansion before turning it on.
	exp := s.expander.Expand(ctx, pk, prompt)
	added := exp.Added
	if added == nil {
		added = []string{}
	}
	errMsg := ""
	if exp.Err != nil {
		errMsg = exp.Err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": pk, "prompt": prompt, "tokens": textutil.Tokenize(prompt, 24),
		"intent": exp.Intent, "expanded": added, "ms": exp.MS,
		"model": s.cfg.ExpandModel, "error": errMsg,
	})
}

type extractReq struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	Source         string `json:"source"`
	// BudgetMS is honoured by /v1/flush only: how long to hold the response
	// before answering "running". 0 means wait as long as it takes.
	BudgetMS int  `json:"budget_ms"`
	Agent    bool `json:"agent"`
}

func (r extractReq) job() Job {
	return Job{SessionID: r.SessionID, TranscriptPath: r.TranscriptPath, CWD: r.CWD, Agent: r.Agent}
}

func (s *server) handleExtract(w http.ResponseWriter, r *http.Request) {
	var req extractReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !validTranscriptPath(req.TranscriptPath, s.cfg.AgentRoots) {
		http.Error(w, "invalid transcript_path", http.StatusBadRequest)
		return
	}
	source := req.Source
	if source != "session_end" {
		source = "stop"
	}
	job := req.job()
	job.Final = source == "session_end"
	s.rememberSource(job)
	s.queue.Notify(source, job)
	// Notify ran first, so the drain below already sees the new due time.
	saved := s.saves.Drain(req.SessionID, s.cfg.HookSavedLines)
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "saved": saved})
}

func (s *server) rememberSource(j Job) {
	if s.touch == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	src := graph.SessionSource{ID: j.SessionID, ProjectKey: project.ResolveKey(j.CWD),
		TranscriptPath: j.TranscriptPath, CWD: j.CWD, Agent: j.Agent}
	if err := s.touch(ctx, src, time.Now().Unix()); err != nil {
		s.log.Warn("session source not recorded", "session", j.SessionID, "err", err)
	}
}

// validTranscriptPath only accepts real files under ~/.claude/projects or a
// configured agent root — the daemon must never be told to read arbitrary files.
func validTranscriptPath(p string, agentRoots []string) bool {
	if p == "" {
		return false
	}
	clean := filepath.Clean(p)
	fi, err := os.Stat(clean)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return fromClaudeProjects(clean) || agentRoot(clean, agentRoots) != ""
}

func (s *server) handleMemories(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	pk := project.ResolveKey(qv.Get("cwd"))
	prompt := qv.Get("q")
	limit := defaultSearchLimit
	if n, err := strconv.Atoi(qv.Get("limit")); err == nil && n > 0 && n <= 50 {
		limit = n
	}
	offset, _ := strconv.Atoi(qv.Get("offset"))
	offset = max(offset, 0)
	// Budget must cover expansion too, or the CLI path times out in exactly
	// the cases where the hook path succeeds.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ExpandBudget()+2*time.Second)
	defer cancel()
	exp := s.expandFor(ctx, pk, prompt)
	// No relevance floor here: imem search is an explicit query capped at
	// `limit` (10 for the agents), and dropping weak matches would only cost
	// the agents recall.
	retr := &retrieve.Retriever{
		Store: s.store, K: offset + limit, MaxContentChars: s.cfg.MaxMemoryContentChars,
		SameProjectBoost: s.cfg.SameProjectBoost, RulesK: 0,
		Corpus: s.corpus.Get, RelatedMinWeight: s.cfg.RelatedMinWeight,
	}
	if len(exp.Added) > 0 {
		retr.Expand = func(tokens []string) []string { return expand.Merge(tokens, exp.Added) }
	}
	scored, err := retr.Query(ctx, pk, prompt, time.Now().Unix())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	scored = pageOf(scored, offset)
	if sid := qv.Get("session_id"); sid != "" && len(scored) > 0 {
		s.injections.Record(sid, toSearched(scored, prompt))
		s.markInjected(scored)
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": pk, "memories": scored})
}

const defaultSearchLimit = 10

func pageOf(scored []retrieve.Scored, offset int) []retrieve.Scored {
	if offset >= len(scored) {
		return []retrieve.Scored{}
	}
	return scored[offset:]
}

// handleEntities lists entities globally, or scoped to one project when a
// cwd query param is given.
func (s *server) handleEntities(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	pk := ""
	if cwd := qv.Get("cwd"); cwd != "" {
		pk = project.ResolveKey(cwd)
	}
	limit := 50
	if n, err := strconv.Atoi(qv.Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	list, err := s.store.EntityList(ctx, pk, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if list == nil {
		list = []graph.EntityInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": pk, "entities": list})
}

func (s *server) handleEntity(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if strings.TrimSpace(name) == "" {
		http.Error(w, "name query param required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	detail, err := s.store.EntityDetail(ctx, name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	stats, err := s.store.Stats(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"memgraph": false, "projects": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memgraph": true, "projects": stats})
}

func (s *server) handleFlush(w http.ResponseWriter, r *http.Request) {
	var req extractReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.TranscriptPath != "" && !validTranscriptPath(req.TranscriptPath, s.cfg.AgentRoots) {
		http.Error(w, "invalid transcript_path", http.StatusBadRequest)
		return
	}
	budget := time.Duration(req.BudgetMS) * time.Millisecond
	if req.TranscriptPath != "" {
		s.rememberSource(req.job())
	}
	done, err := s.queue.FlushBudget(req.job(), budget)
	// Drained AFTER the flush so this run's own report is included. The
	// error travels inside "saved" with a 200: the hook has to render a
	// failed extraction, and a 5xx would read as the daemon being down.
	saved := s.saves.Drain(req.SessionID, s.cfg.HookSavedLines)
	if !done {
		// Still running: the work owns its own context and finishes in the
		// background, so its report drains at the next prompt.
		saved.Status = "running"
	}
	if err != nil && saved.Error == "" {
		saved.Error = clipErr(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": err == nil, "saved": saved})
}

// handleSaved is the read-only view of the save log: it never drains, so a
// debug curl cannot eat the line the user is waiting to see.
func (s *server) handleSaved(w http.ResponseWriter, r *http.Request) {
	if sid := r.URL.Query().Get("session_id"); sid != "" {
		reports, due := s.saves.Peek(sid)
		if reports == nil {
			reports = []SaveReport{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": sid, "reports": reports, "due_in_s": due,
		})
		return
	}
	sessions := s.saves.PeekAll(10)
	if sessions == nil {
		sessions = []SaveReport{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// handleBackup forces a dump now, on top of the scheduled loop.
func (s *server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.BackupEnabled {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "backups disabled in config"})
		return
	}
	res, err := s.runBackup(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if res.Statements == 0 {
		_, lastErr := s.backupStatus()
		if lastErr == "" {
			lastErr = "dump carried no nodes"
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": lastErr + ", nothing written"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": res.Info.Path, "bytes": res.Info.Size, "statements": res.Statements,
	})
}

func (s *server) handleBackups(w http.ResponseWriter, r *http.Request) {
	dir := s.cfg.BackupPath()
	list, err := backup.List(dir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if list == nil {
		list = []backup.Info{}
	}
	_, lastErr := s.backupStatus()
	out := map[string]any{
		"enabled":        s.cfg.BackupEnabled,
		"dir":            dir,
		"interval_hours": s.cfg.BackupIntervalHours,
		"keep":           s.cfg.BackupKeep,
		"backups":        list,
		"last_error":     lastErr,
	}
	if len(list) > 0 {
		out["next_due"] = list[0].ModTime.Add(s.cfg.BackupInterval()).Unix()
	}
	writeJSON(w, http.StatusOK, out)
}
