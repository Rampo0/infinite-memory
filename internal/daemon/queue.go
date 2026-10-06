package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/extract"
	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/project"
	"github.com/Rampo0/infinite-memory/internal/textutil"
)

type Job struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	Agent          bool   `json:"agent,omitempty"`
}

func (j Job) keepAgent(prev Job) Job {
	j.Agent = j.Agent || prev.Agent
	return j
}

// Queue debounces per-session extraction: rapid Stop events coalesce into
// one job after the debounce window; session_end flushes immediately. One
// worker goroutine serializes claude spawns. A Stop arriving mid-run simply
// re-arms the timer, so nothing is missed (the re-run reads the advanced
// cursor). A failed job is requeued once after a minute (transient Memgraph
// blips); beyond that the next hook event retries naturally.
type Queue struct {
	mu       sync.Mutex
	pend     map[string]*pending
	locks    map[string]*sync.Mutex
	retried  map[string]bool
	jobs     chan Job
	stop     chan struct{}
	debounce time.Duration
	process  func(Job) error
	log      *slog.Logger

	// OnPending, when set, fires whenever a job is armed or enqueued so the
	// daemon can tell hooks "extraction is coming in Ns". Set after
	// construction so NewQueue keeps its signature.
	OnPending func(sid string, dueAt int64)
}

type pending struct {
	timer  *time.Timer
	queued bool
	job    Job
}

func NewQueue(debounce time.Duration, process func(Job) error, log *slog.Logger) *Queue {
	return &Queue{
		pend:     map[string]*pending{},
		locks:    map[string]*sync.Mutex{},
		retried:  map[string]bool{},
		jobs:     make(chan Job, 64),
		stop:     make(chan struct{}),
		debounce: debounce,
		process:  process,
		log:      log,
	}
}

func (q *Queue) Start() { go q.worker() }
func (q *Queue) Stop()  { close(q.stop) }

// Notify registers a hook event. source "stop" (re)arms the debounce timer;
// "session_end" enqueues immediately.
func (q *Queue) Notify(source string, j Job) {
	q.mu.Lock()
	p := q.ensureLocked(j.SessionID)
	j = j.keepAgent(p.job)
	p.job = j
	if source == "session_end" {
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		q.enqueueLocked(p)
		q.mu.Unlock()
		q.notifyPending(j.SessionID, time.Now().Unix())
		return
	}
	if p.timer != nil {
		p.timer.Reset(q.debounce)
	} else {
		sid := j.SessionID
		p.timer = time.AfterFunc(q.debounce, func() { q.onTimer(sid) })
	}
	q.mu.Unlock()
	q.notifyPending(j.SessionID, time.Now().Add(q.debounce).Unix())
}

// notifyPending is called with q.mu released: the callback takes the saveLog's
// own lock, and holding both would couple two unrelated lifetimes.
func (q *Queue) notifyPending(sid string, dueAt int64) {
	if q.OnPending != nil {
		q.OnPending(sid, dueAt)
	}
}

// Flush runs the session's job synchronously (verification helper).
func (q *Queue) Flush(j Job) error {
	q.mu.Lock()
	if p := q.pend[j.SessionID]; p != nil {
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		if j.TranscriptPath == "" {
			j = p.job
		}
		j = j.keepAgent(p.job)
	}
	q.mu.Unlock()
	if j.TranscriptPath == "" {
		return fmt.Errorf("no transcript_path for session %s", j.SessionID)
	}
	return q.runLocked(j)
}

// FlushBudget runs the session's job synchronously but stops waiting after
// budget. On timeout the work continues in the background (Process owns its own
// context, not the request's) and ok is false — the caller should report
// "running", not an error.
func (q *Queue) FlushBudget(j Job, budget time.Duration) (ok bool, err error) {
	done := make(chan error, 1)
	go func() { done <- q.Flush(j) }()
	if budget <= 0 {
		return true, <-done
	}
	t := time.NewTimer(budget)
	defer t.Stop()
	select {
	case err := <-done:
		return true, err
	case <-t.C:
		return false, nil
	}
}

func (q *Queue) onTimer(sid string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	p := q.pend[sid]
	if p == nil {
		return
	}
	p.timer = nil
	q.enqueueLocked(p)
}

func (q *Queue) ensureLocked(sid string) *pending {
	p := q.pend[sid]
	if p == nil {
		p = &pending{}
		q.pend[sid] = p
	}
	return p
}

func (q *Queue) enqueueLocked(p *pending) {
	if p.queued {
		return
	}
	select {
	case q.jobs <- p.job:
		p.queued = true
	default:
		q.log.Warn("extract queue full, dropping job", "session", p.job.SessionID)
	}
}

func (q *Queue) enqueue(j Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	p := q.ensureLocked(j.SessionID)
	p.job = j.keepAgent(p.job)
	q.enqueueLocked(p)
}

func (q *Queue) sessionLock(sid string) *sync.Mutex {
	q.mu.Lock()
	defer q.mu.Unlock()
	lk := q.locks[sid]
	if lk == nil {
		lk = &sync.Mutex{}
		q.locks[sid] = lk
	}
	return lk
}

// runLocked serializes work per session (worker vs Flush).
func (q *Queue) runLocked(j Job) error {
	lk := q.sessionLock(j.SessionID)
	lk.Lock()
	defer lk.Unlock()
	return q.process(j)
}

func (q *Queue) worker() {
	for {
		select {
		case <-q.stop:
			return
		case j := <-q.jobs:
			q.mu.Lock()
			if p := q.pend[j.SessionID]; p != nil {
				p.queued = false
			}
			q.mu.Unlock()

			err := q.runLocked(j)

			q.mu.Lock()
			if err != nil && !q.retried[j.SessionID] {
				q.retried[j.SessionID] = true
				q.mu.Unlock()
				time.AfterFunc(time.Minute, func() { q.enqueue(j) })
				continue
			}
			if err == nil {
				delete(q.retried, j.SessionID)
			}
			q.mu.Unlock()
		}
	}
}

// Worker is the extraction pipeline: transcript delta -> prompt -> headless
// claude -> parse -> save -> cursor advance.
type Worker struct {
	Store  *graph.Store
	Runner *extract.Runner
	Cfg    config.Config
	Log    *slog.Logger
	// Saves, when set, records each run's outcome so hooks can print it.
	// Nil-safe, so tests can build a Worker without one.
	Saves *saveLog
	// Similar finds existing memories like a transcript excerpt, for the
	// extractor to reconcile against (B1). Nil skips reconcile.
	Similar func(ctx context.Context, pk string, tokens []string) ([]extract.Known, error)
	// Shown lists memories injected into a session since a unix time, for
	// the extractor to grade (B2). Nil skips feedback.
	Shown func(sid string, since int64) []extract.Known

	mu      sync.Mutex
	fails   map[string]int
	lastRun map[string]int64 // per session: start of the last successful run
}

func NewWorker(store *graph.Store, cfg config.Config, log *slog.Logger) *Worker {
	return &Worker{
		Store:   store,
		Runner:  extract.NewRunner(cfg),
		Cfg:     cfg,
		Log:     log,
		fails:   map[string]int{},
		lastRun: map[string]int64{},
	}
}

const minDeltaChars = 200

// Process runs one extraction and records its outcome for the hooks. The work
// itself lives in process; this wrapper exists so every exit path reports.
func (w *Worker) Process(j Job) error {
	start := time.Now()
	rep, err := w.process(j)
	if rep.quiet {
		return err
	}
	rep.SessionID = j.SessionID
	rep.At = time.Now().Unix()
	rep.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		rep.Error = clipErr(err)
	}
	w.Saves.Record(rep)
	return err
}

// clipErr reduces an error to one short line: it ends up in a CLI message, not
// a log file.
func clipErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:159] + "…"
	}
	return s
}

func (w *Worker) process(j Job) (SaveReport, error) {
	var rep SaveReport
	ctx := context.Background()
	pk := project.ResolveKey(j.CWD)
	now := time.Now().Unix()

	cur64, err := w.Store.GetCursor(ctx, j.SessionID)
	if err != nil {
		return rep, fmt.Errorf("get cursor: %w", err)
	}
	cur := int(cur64)

	turns, total, err := extract.ReadDelta(j.TranscriptPath, cur, w.Cfg.MaxTranscriptChars)
	if err != nil {
		return rep, fmt.Errorf("read transcript: %w", err)
	}
	if cur > total {
		w.Log.Warn("cursor beyond transcript, resetting", "session", j.SessionID, "cursor", cur, "lines", total)
		turns, total, err = extract.ReadDelta(j.TranscriptPath, 0, w.Cfg.MaxTranscriptChars)
		if err != nil {
			return rep, fmt.Errorf("read transcript: %w", err)
		}
	}

	if extract.TotalChars(turns) < minDeltaChars {
		// Quiet: this fires on nearly every short turn, and a "nothing to
		// save" line on each would be pure noise.
		w.Log.Debug("delta too small, skipping spawn", "session", j.SessionID, "lines", total)
		rep.quiet = true
		return rep, w.Store.SetCursor(ctx, j.SessionID, pk, int64(total), now)
	}

	known, err := w.Store.EntityNames(ctx, pk, 20)
	if err != nil {
		w.Log.Warn("entity names fetch failed", "err", err)
		known = nil
	}
	if global, err := w.Store.EntityNamesGlobal(ctx, 15); err == nil {
		seen := make(map[string]bool, len(known))
		for _, n := range known {
			seen[strings.ToLower(n)] = true
		}
		for _, n := range global {
			if !seen[strings.ToLower(n)] {
				known = append(known, n)
			}
		}
	}

	// Fails closed: anything not from ~/.claude/projects is an agent's
	// transcript, even a path that no longer resolves into an agent root since
	// it was validated, and so is any session a hook flagged as headless
	// (claude -p, SDK agents) — the flag only ever demotes. It is extracted
	// isolated, its rules are demoted, and it never reconciles against or
	// grades the user's memories.
	foreign := isForeign(j)

	var pc extract.PromptContext
	if !foreign {
		if w.Similar != nil {
			if sim, err := w.Similar(ctx, pk, deltaTokens(turns)); err == nil {
				pc.Existing = sim
			} else {
				w.Log.Warn("similar memories lookup failed", "err", err)
			}
		}
		if w.Shown != nil {
			w.mu.Lock()
			since := w.lastRun[j.SessionID]
			w.mu.Unlock()
			pc.Shown = w.Shown(j.SessionID, since)
		}
	}
	prompt := extract.BuildPromptWith(pk, known, turns, pc)
	spawnCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	w.Log.Info("extracting", "session", j.SessionID, "turns", len(turns), "from_line", cur, "to_line", total, "model", w.Runner.Model, "isolated", foreign)
	raw, err := w.Runner.Run(spawnCtx, prompt, foreign)
	if err != nil {
		return rep, w.fail(ctx, j.SessionID, pk, total, now, fmt.Errorf("claude: %w", err))
	}
	mems, verdicts, err := planExtraction(raw, foreign, knownIDs(pc.Existing), knownIDs(pc.Shown))
	if err != nil {
		return rep, w.fail(ctx, j.SessionID, pk, total, now, fmt.Errorf("parse: %w", err))
	}
	if len(mems) > 0 {
		saved, err := w.Store.SaveBatch(ctx, pk, j.SessionID, now, mems)
		if err != nil {
			return rep, w.fail(ctx, j.SessionID, pk, total, now, fmt.Errorf("save: %w", err))
		}
		for _, o := range saved {
			rep.Memories = append(rep.Memories, SavedMemory{
				Title: o.Title, Kind: o.Kind, New: o.New, Seen: o.Seen, Updated: o.Updated,
			})
		}
	}
	if len(verdicts) > 0 {
		// Best effort: a lost grade costs ranking, not correctness.
		if err := w.Store.ApplyFeedback(ctx, now, verdicts); err != nil {
			w.Log.Warn("feedback not applied", "err", err)
		} else {
			w.Log.Info("feedback", "session", j.SessionID, "verdicts", len(verdicts))
		}
	}
	if len(rep.Memories) == 0 {
		rep.Skipped = "nothing worth saving"
	}

	w.mu.Lock()
	delete(w.fails, j.SessionID)
	w.lastRun[j.SessionID] = now
	w.mu.Unlock()

	if err := w.Store.SetCursor(ctx, j.SessionID, pk, int64(total), now); err != nil {
		return rep, fmt.Errorf("set cursor: %w", err)
	}
	w.Log.Info("extracted", "session", j.SessionID, "memories", len(mems), "cursor", total)
	return rep, nil
}

func isForeign(j Job) bool {
	return j.Agent || !fromClaudeProjects(j.TranscriptPath)
}

// fail keeps the cursor untouched so the next Stop retries the delta; after
// 3 consecutive failures the delta is skipped (poison-pill guard).
func (w *Worker) fail(ctx context.Context, sid, pk string, total int, now int64, cause error) error {
	w.mu.Lock()
	w.fails[sid]++
	n := w.fails[sid]
	w.mu.Unlock()
	if n >= 3 {
		w.Log.Error("3 consecutive failures, skipping delta", "session", sid, "err", cause)
		w.mu.Lock()
		delete(w.fails, sid)
		w.mu.Unlock()
		if err := w.Store.SetCursor(ctx, sid, pk, int64(total), now); err != nil {
			w.Log.Error("cursor skip failed", "err", err)
		}
		return cause
	}
	w.Log.Warn("extraction failed, will retry", "session", sid, "attempt", n, "err", cause)
	return cause
}

// planExtraction turns the extractor's reply into what to write. Interactive
// transcripts keep reconcile ops whose target the model was shown, and
// feedback on memories the assistant was shown. Agent transcripts (foreign)
// stay add-only with rules and preferences demoted and no feedback: a bot's
// text may add facts, never retire, reinforce or grade the user's memories.
func planExtraction(raw string, foreign bool, existing, shown []string) ([]graph.MemoryIn, []graph.Verdict, error) {
	mems, fb, err := extract.ParseExtraction(raw)
	if err != nil {
		return nil, nil, err
	}
	if foreign {
		mems = extract.ResolveOps(mems, nil)
		demoteAgentKinds(mems)
		return mems, nil, nil
	}
	mems = extract.ResolveOps(mems, toSet(existing))
	allowed := toSet(shown)
	var verdicts []graph.Verdict
	for _, f := range fb {
		if allowed[f.ID] {
			verdicts = append(verdicts, graph.Verdict{ID: f.ID, Verdict: f.Verdict})
		}
	}
	return mems, verdicts, nil
}

// deltaTokens are the excerpt's search terms, newest turns first: what the
// extractor will write about is most likely in the latest exchange.
func deltaTokens(turns []extract.Turn) []string {
	var b strings.Builder
	for i := len(turns) - 1; i >= 0; i-- {
		b.WriteString(turns[i].Text)
		b.WriteString("\n")
	}
	return textutil.Tokenize(b.String(), 300)
}

func knownIDs(ks []extract.Known) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.ID
	}
	return out
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
