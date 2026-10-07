package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Rampo0/infinite-memory/internal/config"
)

// injected is one memory a hook put into a session's context.
type injected struct {
	ID      string
	Title   string
	Content string
	Kind    string
	At      int64
	Via     string
	Query   string
}

const viaSearch = "search"

// injectionLog remembers, per session, which memories UserPromptSubmit has
// already injected. Retrieve skips them (they are still in the model's
// context), and the extractor is told which ones the assistant was shown.
// In memory only: after a daemon restart a memory may be injected once more.
type injectionLog struct {
	path       string
	timer      *time.Timer
	mu         sync.Mutex
	bySess     map[string]*sessInjections
	maxPerSess int
	ttl        time.Duration
	now        func() int64
}

type sessInjections struct {
	items []injected
	last  int64
}

func newInjectionLog() *injectionLog {
	return &injectionLog{
		bySess:     map[string]*sessInjections{},
		maxPerSess: 200,
		ttl:        24 * time.Hour,
		now:        func() int64 { return time.Now().Unix() },
	}
}

// Record appends what a prompt injected, keeping the newest maxPerSess, and
// evicts sessions idle past the TTL.
func (l *injectionLog) Record(sid string, items []injected) {
	if l == nil || sid == "" || len(items) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, ss := range l.bySess {
		if now-ss.last > int64(l.ttl/time.Second) {
			delete(l.bySess, k)
		}
	}
	ss := l.bySess[sid]
	if ss == nil {
		ss = &sessInjections{}
		l.bySess[sid] = ss
	}
	for _, it := range items {
		it.At = now
		ss.items = append(ss.items, it)
	}
	if over := len(ss.items) - l.maxPerSess; over > 0 {
		ss.items = append([]injected(nil), ss.items[over:]...)
	}
	ss.last = now
	l.schedule()
}

// Seen is the set of memory ids already injected into the session.
func (l *injectionLog) Seen(sid string) map[string]bool {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ss := l.bySess[sid]
	if ss == nil {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(ss.items))
	for _, it := range ss.items {
		if it.Via != viaSearch {
			out[it.ID] = true
		}
	}
	return out
}

// Since returns the injections at or after unix time at, oldest first.
func (l *injectionLog) Since(sid string, at int64) []injected {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ss := l.bySess[sid]
	if ss == nil {
		return nil
	}
	var out []injected
	for _, it := range ss.items {
		if it.At >= at {
			out = append(out, it)
		}
	}
	return out
}

// Reset forgets a session: after /clear or a compaction the earlier
// injections are no longer in the model's context.
func (l *injectionLog) Reset(sid string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.bySess, sid)
	l.schedule()
}

type savedSession struct {
	Items []injected `json:"items"`
	Last  int64      `json:"last"`
}

func (l *injectionLog) schedule() {
	if l.path == "" || l.timer != nil {
		return
	}
	l.timer = time.AfterFunc(2*time.Second, l.flush)
}

func (l *injectionLog) flush() {
	l.mu.Lock()
	l.timer = nil
	out := make(map[string]savedSession, len(l.bySess))
	for sid, ss := range l.bySess {
		out[sid] = savedSession{Items: ss.items, Last: ss.last}
	}
	path := l.path
	l.mu.Unlock()
	if path == "" {
		return
	}
	if data, err := json.Marshal(out); err == nil {
		_ = writeFileAtomic(path, string(data))
	}
}

func (l *injectionLog) load(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var in map[string]savedSession
	if json.Unmarshal(data, &in) != nil {
		return
	}
	now := l.now()
	for sid, ss := range in {
		if now-ss.Last <= int64(l.ttl/time.Second) {
			l.bySess[sid] = &sessInjections{items: ss.Items, last: ss.Last}
		}
	}
}

func loadInjectionLog(cfg config.Config) *injectionLog {
	l := newInjectionLog()
	l.load(filepath.Join(cfg.StateDir(), "injections.json"))
	return l
}
