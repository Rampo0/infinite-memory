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

	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/project"
	"github.com/Rampo0/infinite-memory/internal/retrieve"
)

type server struct {
	cfg   config.Config
	store *graph.Store
	retr  *retrieve.Retriever
	queue *Queue
	log   *slog.Logger

	schemaMu sync.Mutex
	schemaOK bool
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
		retr:  &retrieve.Retriever{Store: store, K: cfg.RetrieveK, MaxContentChars: cfg.MaxMemoryContentChars},
		log:   log,
	}
	worker := NewWorker(store, cfg, log)
	s.queue = NewQueue(cfg.Debounce(), worker.Process, log)
	s.queue.Start()

	// Memgraph may be down at startup: log and keep serving, the schema is
	// re-attempted on the next successful health check.
	s.ensureSchema(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/retrieve", s.handleRetrieve)
	mux.HandleFunc("POST /v1/extract", s.handleExtract)
	mux.HandleFunc("GET /v1/memories", s.handleMemories)
	mux.HandleFunc("GET /v1/stats", s.handleStats)
	mux.HandleFunc("POST /v1/flush", s.handleFlush)

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
	empty := map[string]any{"context": "", "count": 0}
	var req retrieveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	pk := project.ResolveKey(req.CWD)
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RetrieveTO())
	defer cancel()
	start := time.Now()
	block, count, err := s.retr.Retrieve(ctx, pk, req.Prompt, time.Now().Unix())
	if err != nil {
		s.log.Warn("retrieve failed", "err", err, "project", pk)
		writeJSON(w, http.StatusOK, empty)
		return
	}
	s.log.Info("retrieve", "project", pk, "count", count, "ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, map[string]any{"context": block, "count": count})
}

type extractReq struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	Source         string `json:"source"`
}

func (s *server) handleExtract(w http.ResponseWriter, r *http.Request) {
	var req extractReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !validTranscriptPath(req.TranscriptPath) {
		http.Error(w, "invalid transcript_path", http.StatusBadRequest)
		return
	}
	source := req.Source
	if source != "session_end" {
		source = "stop"
	}
	s.queue.Notify(source, Job{SessionID: req.SessionID, TranscriptPath: req.TranscriptPath, CWD: req.CWD})
	w.WriteHeader(http.StatusAccepted)
}

// validTranscriptPath only accepts real files under ~/.claude/projects —
// the daemon must never be told to read arbitrary files.
func validTranscriptPath(p string) bool {
	if p == "" {
		return false
	}
	clean := filepath.Clean(p)
	root := config.ClaudeProjectsDir() + string(filepath.Separator)
	if !strings.HasPrefix(clean, root) {
		return false
	}
	fi, err := os.Stat(clean)
	return err == nil && fi.Mode().IsRegular()
}

func (s *server) handleMemories(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	pk := project.ResolveKey(qv.Get("cwd"))
	prompt := qv.Get("q")
	limit := s.cfg.RetrieveK
	if n, err := strconv.Atoi(qv.Get("limit")); err == nil && n > 0 && n <= 50 {
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	retr := &retrieve.Retriever{Store: s.store, K: limit, MaxContentChars: s.cfg.MaxMemoryContentChars}
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
	if req.TranscriptPath != "" && !validTranscriptPath(req.TranscriptPath) {
		http.Error(w, "invalid transcript_path", http.StatusBadRequest)
		return
	}
	err := s.queue.Flush(Job{SessionID: req.SessionID, TranscriptPath: req.TranscriptPath, CWD: req.CWD})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
