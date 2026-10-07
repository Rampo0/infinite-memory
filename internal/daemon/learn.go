package daemon

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/textutil"
)

const maxLearnedPerRun = 3

type learnResult struct {
	Aliases    map[string][]string
	Used       int
	SearchOnly int
}

func learnAliases(shown []injected, verdicts []graph.Verdict) learnResult {
	res := learnResult{Aliases: map[string][]string{}}
	used := map[string]bool{}
	for _, v := range verdicts {
		if v.Verdict == "used" {
			used[v.ID] = true
		}
	}
	hooked, searched := map[string]bool{}, map[string]injected{}
	for _, it := range shown {
		if it.Via == viaSearch {
			searched[it.ID] = it
		} else {
			hooked[it.ID] = true
		}
	}
	for id := range used {
		res.Used++
		it, ok := searched[id]
		if !ok || hooked[id] {
			continue
		}
		res.SearchOnly++
		if terms := novelTerms(it); len(terms) > 0 {
			res.Aliases[id] = terms
		}
	}
	return res
}

func novelTerms(it injected) []string {
	have := map[string]bool{}
	for _, t := range textutil.Tokenize(it.Title+" "+it.Content, 200) {
		have[t] = true
	}
	var out []string
	for _, t := range textutil.Tokenize(it.Query, 24) {
		if len(t) < 3 || have[t] {
			continue
		}
		out = append(out, t)
		if len(out) == maxLearnedPerRun {
			break
		}
	}
	return out
}

type learnStats struct {
	Used       int `json:"used"`
	SearchOnly int `json:"search_only"`
	Learned    int `json:"learned"`
}

type learnEvent struct {
	at int64
	learnStats
}

type learnLog struct {
	mu     sync.Mutex
	events []learnEvent
	now    func() int64
}

func newLearnLog() *learnLog { return &learnLog{now: func() int64 { return time.Now().Unix() }} }

func (l *learnLog) Record(r learnResult) {
	learned := 0
	for _, terms := range r.Aliases {
		learned += len(terms)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now() - int64(gateWindow/time.Second)
	kept := l.events[:0]
	for _, e := range l.events {
		if e.at >= cutoff {
			kept = append(kept, e)
		}
	}
	l.events = append(kept, learnEvent{at: l.now(), learnStats: learnStats{r.Used, r.SearchOnly, learned}})
}

func (l *learnLog) Stats(since int64) learnStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	var st learnStats
	for _, e := range l.events {
		if e.at >= since {
			st.Used += e.Used
			st.SearchOnly += e.SearchOnly
			st.Learned += e.Learned
		}
	}
	return st
}

func (s *server) learnFrom(r learnResult) {
	s.learning.Record(r)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for id, terms := range r.Aliases {
		if err := s.store.LearnAliases(ctx, id, terms); err != nil {
			s.log.Warn("aliases not learned", "memory", id, "err", err)
			continue
		}
		s.log.Info("aliases learned", "memory", id, "terms", terms)
	}
}

func (s *server) handleLearningStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.learning.Stats(time.Now().Add(-gateWindow).Unix()))
}
