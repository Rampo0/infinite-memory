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
	Final          bool   `json:"final,omitempty"`
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
	if source == "session_end" || source == "sweep" {
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
type workerStore interface {
	GetCursor(ctx context.Context, sid string) (int64, error)
	SetCursor(ctx context.Context, sid, pk string, line, now int64) error
	EntityNames(ctx context.Context, pk string, limit int) ([]string, error)
	EntityNamesGlobal(ctx context.Context, limit int) ([]string, error)
	SaveBatch(ctx context.Context, pk, sid string, now int64, mems []graph.MemoryIn) ([]graph.SaveOutcome, error)
	ApplyFeedback(ctx context.Context, now int64, vs []graph.Verdict) error
}

type Worker struct {
	Store  workerStore
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

	Spawn   func(ctx context.Context, prompt string) (string, error)
	Usage   func(ctx context.Context) (float64, error)
	Requeue func(j Job)

	mu       sync.Mutex
	fails    map[string]int
	lastRun  map[string]int64 // per session: start of the last successful run
	cooldown map[string]int64
	shrink   map[string]int
}

func NewWorker(store *graph.Store, cfg config.Config, log *slog.Logger) *Worker {
	runner := extract.NewRunner(cfg)
	return &Worker{
		Store:    store,
		Runner:   runner,
		Cfg:      cfg,
		Log:      log,
		Spawn:    runner.Run,
		Usage:    cachedUsage(usageProbe(runner, cfg.ExpandModel), 10*time.Minute, time.Now),
		fails:    map[string]int{},
		lastRun:  map[string]int64{},
		cooldown: map[string]int64{},
		shrink:   map[string]int{},
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

type step int

const (
	stepAdvance step = iota
	stepDefer
	stepExtract
)

const (
	failsBeforeCooldown = 3
	baseCooldown        = 30 * time.Minute
	maxCooldown         = 24 * time.Hour
)

type attempt struct {
	j     Job
	pk    string
	chunk extract.Chunk
	from  int
	now   int64
}

func planDelta(c extract.Chunk, j Job) step {
	if len(c.Turns) == 0 {
		return stepAdvance
	}
	if extract.TotalChars(c.Turns) < minDeltaChars && !j.Final && !j.Agent {
		return stepDefer
	}
	return stepExtract
}

func (w *Worker) process(j Job) (SaveReport, error) {
	var rep SaveReport
	ctx := context.Background()
	if msg := w.coolingDown(j.SessionID); msg != "" {
		rep.Error = msg
		return rep, nil
	}
	a, err := w.readAttempt(ctx, j)
	if err != nil {
		return rep, err
	}
	switch planDelta(a.chunk, j) {
	case stepAdvance:
		rep.quiet = true
		return rep, w.Store.SetCursor(ctx, j.SessionID, a.pk, int64(a.chunk.Next), a.now)
	case stepDefer:
		w.Log.Info("short delta deferred", "session", j.SessionID, "chars", extract.TotalChars(a.chunk.Turns))
		rep.quiet = true
		return rep, nil
	}
	if reason := w.overUsage(ctx); reason != "" {
		w.Log.Info("extraction deferred", "session", j.SessionID, "reason", reason)
		rep.Skipped = reason
		return rep, nil
	}
	return w.extract(ctx, a)
}

func (w *Worker) readAttempt(ctx context.Context, j Job) (attempt, error) {
	a := attempt{j: j, pk: project.ResolveKey(j.CWD), now: time.Now().Unix()}
	cur, err := w.Store.GetCursor(ctx, j.SessionID)
	if err != nil {
		return a, fmt.Errorf("get cursor: %w", err)
	}
	a.from = int(cur)
	budget := w.budget(j.SessionID)
	a.chunk, err = extract.ReadChunk(j.TranscriptPath, a.from, budget)
	if err == nil && a.from > a.chunk.Total {
		w.Log.Warn("cursor beyond transcript, resetting", "session", j.SessionID, "cursor", a.from, "lines", a.chunk.Total)
		a.from = 0
		a.chunk, err = extract.ReadChunk(j.TranscriptPath, 0, budget)
	}
	if err != nil {
		return a, fmt.Errorf("read transcript: %w", err)
	}
	return a, nil
}

func (w *Worker) budget(sid string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if b, ok := w.shrink[sid]; ok {
		return b
	}
	return w.Cfg.MaxTranscriptChars
}

func (w *Worker) extract(ctx context.Context, a attempt) (SaveReport, error) {
	var rep SaveReport
	foreign := isForeign(a.j)
	pc := w.promptContext(ctx, a, foreign)
	prompt := extract.BuildPromptWith(a.pk, w.knownEntities(ctx, a.pk), a.chunk.Turns, pc)
	spawnCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	w.Log.Info("extracting", "session", a.j.SessionID, "turns", len(a.chunk.Turns), "from_line", a.from,
		"to_line", a.chunk.Next, "lines", a.chunk.Total, "model", w.Cfg.ExtractModel, "agent", foreign)
	raw, err := w.Spawn(spawnCtx, prompt)
	if err != nil {
		return rep, w.fail(a, fmt.Errorf("claude: %w", err))
	}
	mems, verdicts, err := planExtraction(raw, foreign, knownIDs(pc.Existing), knownIDs(pc.Shown))
	if err != nil {
		return rep, w.fail(a, fmt.Errorf("parse: %w", err))
	}
	rep, err = w.persist(ctx, a, mems, verdicts)
	if err != nil {
		return rep, w.fail(a, err)
	}
	return rep, w.advance(ctx, a, len(mems))
}

func (w *Worker) knownEntities(ctx context.Context, pk string) []string {
	known, err := w.Store.EntityNames(ctx, pk, 20)
	if err != nil {
		w.Log.Warn("entity names fetch failed", "err", err)
		known = nil
	}
	global, err := w.Store.EntityNamesGlobal(ctx, 15)
	if err != nil {
		return known
	}
	seen := make(map[string]bool, len(known))
	for _, n := range known {
		seen[strings.ToLower(n)] = true
	}
	for _, n := range global {
		if !seen[strings.ToLower(n)] {
			known = append(known, n)
		}
	}
	return known
}

func (w *Worker) promptContext(ctx context.Context, a attempt, foreign bool) extract.PromptContext {
	var pc extract.PromptContext
	if foreign {
		return pc
	}
	if w.Similar != nil {
		if sim, err := w.Similar(ctx, a.pk, deltaTokens(a.chunk.Turns)); err == nil {
			pc.Existing = sim
		} else {
			w.Log.Warn("similar memories lookup failed", "err", err)
		}
	}
	if w.Shown != nil {
		w.mu.Lock()
		since := w.lastRun[a.j.SessionID]
		w.mu.Unlock()
		pc.Shown = w.Shown(a.j.SessionID, since)
	}
	return pc
}

func (w *Worker) persist(ctx context.Context, a attempt, mems []graph.MemoryIn, verdicts []graph.Verdict) (SaveReport, error) {
	var rep SaveReport
	if len(mems) > 0 {
		saved, err := w.Store.SaveBatch(ctx, a.pk, a.j.SessionID, a.now, mems)
		if err != nil {
			return rep, fmt.Errorf("save: %w", err)
		}
		for _, o := range saved {
			rep.Memories = append(rep.Memories, SavedMemory{
				Title: o.Title, Kind: o.Kind, New: o.New, Seen: o.Seen, Updated: o.Updated,
			})
		}
	}
	if len(verdicts) > 0 {
		if err := w.Store.ApplyFeedback(ctx, a.now, verdicts); err != nil {
			w.Log.Warn("feedback not applied", "err", err)
		} else {
			w.Log.Info("feedback", "session", a.j.SessionID, "verdicts", len(verdicts))
		}
	}
	if len(rep.Memories) == 0 {
		rep.Skipped = "nothing worth saving"
	}
	return rep, nil
}

func (w *Worker) advance(ctx context.Context, a attempt, saved int) error {
	sid := a.j.SessionID
	w.mu.Lock()
	delete(w.fails, sid)
	delete(w.shrink, sid)
	delete(w.cooldown, sid)
	w.lastRun[sid] = a.now
	w.mu.Unlock()
	if err := w.Store.SetCursor(ctx, sid, a.pk, int64(a.chunk.Next), a.now); err != nil {
		return fmt.Errorf("set cursor: %w", err)
	}
	w.Log.Info("extracted", "session", sid, "memories", saved, "cursor", a.chunk.Next, "lines", a.chunk.Total)
	if a.chunk.Next < a.chunk.Total && w.Requeue != nil {
		w.Requeue(a.j)
	}
	return nil
}

func (w *Worker) overUsage(ctx context.Context) string {
	if w.Usage == nil || w.Cfg.ExtractMaxUsage <= 0 {
		return ""
	}
	u, err := w.Usage(ctx)
	if err != nil || u < w.Cfg.ExtractMaxUsage {
		return ""
	}
	return fmt.Sprintf("deferred: 5-hour usage %.0f%% is over extract_max_usage %.0f%%", u*100, w.Cfg.ExtractMaxUsage*100)
}

func (w *Worker) coolingDown(sid string) string {
	w.mu.Lock()
	until, n := w.cooldown[sid], w.fails[sid]
	w.mu.Unlock()
	if until == 0 || time.Now().Unix() >= until {
		return ""
	}
	return fmt.Sprintf("saving paused after %d failed extractions, next try %s", n, time.Unix(until, 0).Format("15:04"))
}

func (w *Worker) fail(a attempt, cause error) error {
	sid := a.j.SessionID
	w.mu.Lock()
	w.fails[sid]++
	n := w.fails[sid]
	w.shrink[sid] = max(extract.TotalChars(a.chunk.Turns)/2, 1)
	if n >= failsBeforeCooldown {
		w.cooldown[sid] = a.now + int64(cooldownFor(n)/time.Second)
	}
	w.mu.Unlock()
	w.Log.Warn("extraction failed, cursor kept", "session", sid, "attempt", n, "turns", len(a.chunk.Turns), "err", cause)
	return cause
}

func cooldownFor(fails int) time.Duration {
	return min(baseCooldown<<min(fails-failsBeforeCooldown, 6), maxCooldown)
}

// Fails closed: anything not from ~/.claude/projects is an agent's
// transcript, even a path that no longer resolves into an agent root since
// it was validated, and so is any session a hook flagged as headless
// (claude -p, SDK agents) — the flag only ever demotes. It is extracted
// isolated, its rules are demoted, and it never reconciles against or
// grades the user's memories.
func isForeign(j Job) bool {
	return j.Agent || !fromClaudeProjects(j.TranscriptPath)
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
