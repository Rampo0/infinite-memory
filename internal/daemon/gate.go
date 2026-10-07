package daemon

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const gateWindow = 24 * time.Hour

type gateStats struct {
	Prompts  int `json:"prompts"`
	Searched int `json:"searched"`
	Fired    int `json:"fired"`
}

type gateTurn struct {
	at       int64
	searched bool
	fired    bool
}

type gateLog struct {
	mu    sync.Mutex
	turns map[string]*gateTurn
	now   func() int64
}

func newGateLog() *gateLog {
	return &gateLog{turns: map[string]*gateTurn{}, now: func() int64 { return time.Now().Unix() }}
}

func (g *gateLog) Check(sid, promptID string, searched bool) bool {
	if promptID == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked()
	key := sid + "\x00" + promptID
	t := g.turns[key]
	if t == nil {
		t = &gateTurn{at: g.now()}
		g.turns[key] = t
	}
	t.searched = t.searched || searched
	fire := !t.searched && !t.fired
	t.fired = t.fired || fire
	return fire
}

func (g *gateLog) pruneLocked() {
	cutoff := g.now() - int64(gateWindow/time.Second)
	for k, t := range g.turns {
		if t.at < cutoff {
			delete(g.turns, k)
		}
	}
}

func (g *gateLog) Stats(since int64) gateStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	var st gateStats
	for _, t := range g.turns {
		if t.at < since {
			continue
		}
		st.Prompts++
		if t.searched {
			st.Searched++
		}
		if t.fired {
			st.Fired++
		}
	}
	return st
}

type gateReq struct {
	SessionID string `json:"session_id"`
	PromptID  string `json:"prompt_id"`
	Searched  bool   `json:"searched"`
}

func (s *server) handleGate(w http.ResponseWriter, r *http.Request) {
	var req gateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"fire": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fire": s.gates.Check(req.SessionID, req.PromptID, req.Searched)})
}

func (s *server) handleGateStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.gates.Stats(time.Now().Add(-gateWindow).Unix()))
}
