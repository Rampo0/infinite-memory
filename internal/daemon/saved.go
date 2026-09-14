package daemon

import (
	"sync"
	"time"

	"github.com/Rampo0/infinite-memory/internal/retrieve"
)

// SavedMemory is one memory a batch wrote, flattened for display.
type SavedMemory struct {
	Title string `json:"title"`
	Kind  string `json:"kind"`
	New   bool   `json:"new"`
	Seen  int64  `json:"seen"`
}

// SaveReport is the outcome of one extraction run for one session. At most one
// of Memories / Skipped / Error carries the story.
type SaveReport struct {
	SessionID  string        `json:"session_id"`
	At         int64         `json:"at"` // unix seconds, finish time
	DurationMS int64         `json:"duration_ms"`
	Memories   []SavedMemory `json:"memories"`
	Skipped    string        `json:"skipped"` // "" | "nothing worth saving"
	Error      string        `json:"error"`   // "" on success

	// quiet marks a run not worth reporting at all (the sub-minDeltaChars
	// skip), and reported marks one already drained. Neither is serialized.
	quiet    bool
	reported bool
}

// SavedPayload is the wire shape /v1/retrieve, /v1/flush and /v1/extract all
// carry. Summary is pre-rendered by the daemon; the hook only adds the header.
type SavedPayload struct {
	Summary string `json:"summary"`
	Count   int    `json:"count"` // memories written across drained batches
	New     int    `json:"new"`   // of those, newly created
	Batches int    `json:"batches"`
	MS      int64  `json:"ms"`     // newest drained batch duration
	Status  string `json:"status"` // "" | "running" | "skipped"
	Error   string `json:"error"`  // newest failure, "" when fine
	DueInS  int    `json:"due_in_s"`
}

// saveLog keeps recent extraction outcomes per session so the hooks can print
// what the background worker did. Deliberately in-memory and bounded: a daemon
// restart loses it, which is correct — nobody wants a week-old "saved" line.
// The worker goroutine writes, HTTP handlers read.
type saveLog struct {
	mu      sync.Mutex
	bySess  map[string]*sessionSaves
	max     int           // reports kept per session
	ttl     time.Duration // reports older than this never surface
	maxSess int           // hard cap on tracked sessions
	now     func() int64  // injectable for tests
}

type sessionSaves struct {
	reports   []SaveReport // oldest first, len <= max
	dropped   int          // ring overflow, surfaced as extra "… N more"
	dueAt     int64        // debounce fire time, 0 when idle
	lastTouch int64
}

func newSaveLog() *saveLog {
	return &saveLog{
		bySess:  map[string]*sessionSaves{},
		max:     4,
		ttl:     30 * time.Minute,
		maxSess: 64,
		now:     func() int64 { return time.Now().Unix() },
	}
}

// Record stores one finished run. A quiet report (delta too small to spawn) is
// dropped: it fires on nearly every short turn and would print constant noise.
func (l *saveLog) Record(rep SaveReport) {
	if l == nil || rep.quiet || rep.SessionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ss := l.ensureLocked(rep.SessionID, now)
	ss.dueAt = 0 // the report supersedes any pending state
	ss.reports = append(ss.reports, rep)
	if over := len(ss.reports) - l.max; over > 0 {
		ss.reports = append(ss.reports[:0], ss.reports[over:]...)
		ss.dropped += over
	}
	l.sweepLocked(now)
}

// MarkPending notes that extraction is scheduled, so a hook with nothing to
// drain can still say "extracting in Ns" instead of staying silent.
func (l *saveLog) MarkPending(sid string, dueAt int64) {
	if l == nil || sid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureLocked(sid, l.now()).dueAt = dueAt
}

// Drain returns the session's un-reported, non-expired reports and marks them
// reported, so the same batch never prints twice across the two hooks.
func (l *saveLog) Drain(sid string, maxLines int) SavedPayload {
	var out SavedPayload
	if l == nil || sid == "" {
		return out
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ss := l.bySess[sid]
	if ss == nil {
		return out
	}
	now := l.now()
	ss.lastTouch = now

	var lines []retrieve.SavedLine
	for i := range ss.reports {
		r := &ss.reports[i]
		if r.reported || l.expiredLocked(r.At, now) {
			continue
		}
		r.reported = true
		out.Batches++
		out.MS = r.DurationMS
		if r.Error != "" {
			out.Error = r.Error
		}
		if r.Skipped != "" && out.Status == "" {
			out.Status = "skipped"
		}
		for _, m := range r.Memories {
			lines = append(lines, retrieve.SavedLine{
				Title: m.Title, Kind: m.Kind, New: m.New, Seen: m.Seen,
			})
			out.Count++
			if m.New {
				out.New++
			}
		}
	}
	if out.Count > 0 {
		out.Status = ""
		out.Summary = retrieve.FormatSaved(lines, maxLines)
	}
	if ss.dueAt > now {
		out.DueInS = int(ss.dueAt - now)
	}
	return out
}

// Peek returns a session's reports without marking anything reported, so a
// debug curl never eats the user's pending line.
func (l *saveLog) Peek(sid string) ([]SaveReport, int) {
	if l == nil {
		return nil, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ss := l.bySess[sid]
	if ss == nil {
		return nil, 0
	}
	out := make([]SaveReport, len(ss.reports))
	copy(out, ss.reports)
	due := 0
	if now := l.now(); ss.dueAt > now {
		due = int(ss.dueAt - now)
	}
	return out, due
}

// PeekAll returns the newest report per session, newest first, for `imem
// status`. Never mutates.
func (l *saveLog) PeekAll(limit int) []SaveReport {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []SaveReport
	for _, ss := range l.bySess {
		if n := len(ss.reports); n > 0 {
			out = append(out, ss.reports[n-1])
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].At > out[i].At {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (l *saveLog) ensureLocked(sid string, now int64) *sessionSaves {
	ss := l.bySess[sid]
	if ss == nil {
		ss = &sessionSaves{}
		l.bySess[sid] = ss
	}
	ss.lastTouch = now
	return ss
}

func (l *saveLog) expiredLocked(at, now int64) bool {
	return at > 0 && now-at > int64(l.ttl/time.Second)
}

// sweepLocked drops sessions whose last activity is past the TTL, then evicts
// the least recently touched until the session cap holds.
func (l *saveLog) sweepLocked(now int64) {
	for sid, ss := range l.bySess {
		if ss.dueAt == 0 && l.expiredLocked(ss.lastTouch, now) {
			delete(l.bySess, sid)
		}
	}
	for len(l.bySess) > l.maxSess {
		oldest, oldestAt := "", int64(0)
		for sid, ss := range l.bySess {
			if oldest == "" || ss.lastTouch < oldestAt {
				oldest, oldestAt = sid, ss.lastTouch
			}
		}
		delete(l.bySess, oldest)
	}
}
