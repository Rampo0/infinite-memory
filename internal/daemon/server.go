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
	logPath := cfg.LogPath()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	var w io.Writer = os.Stderr
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		w = io.MultiWriter(os.Stderr, f)
	}
	log := slog.New(slog.NewTextHandler(w, nil))

	store, err := graph.New(cfg.MemgraphURI, cfg.MemgraphUser, cfg.MemgraphPass)
	if err != nil {
		return err
	}

	s := &server{
		cfg:   cfg,
		store: store,
		retr: &retrieve.Retriever{
			Store: store, K: cfg.RetrieveK, MaxContentChars: cfg.MaxMemoryContentChars,
			SameProjectBoost: cfg.SameProjectBoost, RulesK: cfg.RulesK,
			SummaryLines: cfg.HookSummaryLines,
		},
		log: log,
	}
	s.saves = newSaveLog()
	s.vocab = newVocabCache()
	worker := NewWorker(store, cfg, log)
	worker.Saves = s.saves
	s.expander = s.newExpander(worker.Runner)
	s.queue = NewQueue(cfg.Debounce(), worker.Process, log)
	s.queue.OnPending = s.saves.MarkPending
	s.queue.Start()

	if cfg.BackupEnabled {
		s.startBackupLoop()
	}

	// Memgraph may be down at startup: log and keep serving, the schema is
	// re-attempted on the next successful health check.
	s.ensureSchema(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/retrieve", s.handleRetrieve)
	mux.HandleFunc("POST /v1/extract", s.handleExtract)
	mux.HandleFunc("GET /v1/memories", s.handleMemories)
	mux.HandleFunc("GET /v1/expand", s.handleExpand)
	mux.HandleFunc("GET /v1/entities", s.handleEntities)
	mux.HandleFunc("GET /v1/entity", s.handleEntity)
	mux.HandleFunc("GET /v1/stats", s.handleStats)
	mux.HandleFunc("GET /v1/saved", s.handleSaved)
	mux.HandleFunc("POST /v1/flush", s.handleFlush)
	mux.HandleFunc("POST /v1/backup", s.handleBackup)
	mux.HandleFunc("GET /v1/backups", s.handleBackups)

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("bind %s failed (daemon already running?): %w", cfg.HTTPAddr, err)
	}
	log.Info("imem daemon listening", "addr", cfg.HTTPAddr, "memgraph", cfg.MemgraphURI)
	return http.Serve(ln, mux)
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
}

// handleRetrieve always answers 200: failures fail open into empty context.
func (s *server) handleRetrieve(w http.ResponseWriter, r *http.Request) {
	empty := map[string]any{"context": "", "count": 0, "summary": "", "memories": 0, "rules": 0}
	var req retrieveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	// Drained once, up front, and attached to EVERY response below: a
	// Memgraph-down retrieve must not swallow the user's save report.
	saved := s.saves.Drain(req.SessionID, s.cfg.HookSavedLines)
	empty["saved"] = saved
	pk := project.ResolveKey(req.CWD)
	start := time.Now()

	// Expansion spends its own budget first; the graph deadline below is
	// untouched, so a slow or failed spawn costs time but never correctness.
	exp := s.expandFor(r.Context(), pk, req.Prompt)

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RetrieveTO())
	defer cancel()
	retr := *s.retr
	if len(exp.Added) > 0 {
		retr.Expand = func(tokens []string) []string { return expand.Merge(tokens, exp.Added) }
	}
	res, err := retr.Retrieve(ctx, pk, req.Prompt, time.Now().Unix())
	if err != nil {
		s.log.Warn("retrieve failed", "err", err, "project", pk)
		writeJSON(w, http.StatusOK, empty)
		return
	}
	s.log.Info("retrieve", "project", pk, "count", res.Memories+res.Rules,
		"memories", res.Memories, "rules", res.Rules,
		"expanded", len(exp.Added), "llm_ms", exp.MS, "ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, map[string]any{
		"context": res.Block, "count": res.Memories + res.Rules,
		"summary": res.Summary, "memories": res.Memories, "rules": res.Rules,
		"saved": saved, "expanded": expandedLine(exp),
	})
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
	BudgetMS int `json:"budget_ms"`
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
	s.queue.Notify(source, Job{SessionID: req.SessionID, TranscriptPath: req.TranscriptPath, CWD: req.CWD})
	// Notify ran first, so the drain below already sees the new due time.
	saved := s.saves.Drain(req.SessionID, s.cfg.HookSavedLines)
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "saved": saved})
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
	limit := s.cfg.RetrieveK
	if n, err := strconv.Atoi(qv.Get("limit")); err == nil && n > 0 && n <= 50 {
		limit = n
	}
	// Budget must cover expansion too, or the CLI path times out in exactly
	// the cases where the hook path succeeds.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ExpandBudget()+2*time.Second)
	defer cancel()
	exp := s.expandFor(ctx, pk, prompt)
	retr := &retrieve.Retriever{
		Store: s.store, K: limit, MaxContentChars: s.cfg.MaxMemoryContentChars,
		SameProjectBoost: s.cfg.SameProjectBoost, RulesK: 0,
	}
	if len(exp.Added) > 0 {
		retr.Expand = func(tokens []string) []string { return expand.Merge(tokens, exp.Added) }
	}
	scored, err := retr.Query(ctx, pk, prompt, time.Now().Unix())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if scored == nil {
		scored = []retrieve.Scored{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": pk, "memories": scored})
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
	done, err := s.queue.FlushBudget(
		Job{SessionID: req.SessionID, TranscriptPath: req.TranscriptPath, CWD: req.CWD}, budget)
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
