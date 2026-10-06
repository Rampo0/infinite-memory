package eval

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/retrieve"
)

type hits struct {
	mu    sync.Mutex
	paths []string
	sids  []string
}

func (h *hits) add(path, sid string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paths = append(h.paths, path)
	h.sids = append(h.sids, sid)
}

func hookServer(t *testing.T, withPreview bool, ctxBlock string, h *hits) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.add(r.URL.Path, req.SessionID)
		_ = json.NewEncoder(w).Encode(map[string]any{"context": ctxBlock})
	}
	if withPreview {
		mux.HandleFunc("POST /v1/retrieve/preview", handler)
	}
	mux.HandleFunc("POST /v1/retrieve", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// An oversized block on a daemon without the preview endpoint: the runner
// falls back to /v1/retrieve under a session id no hook uses, and scores only
// the 2KB preview the model would see.
func TestRunnerHookFallsBackAndScoresOnlyThePreview(t *testing.T) {
	early := "- [fact] Early title — " + strings.Repeat("a", 100) + " (1d ago)"
	late := "- [fact] Late title — c (1d ago)"
	big := block(early, strings.Repeat("- [fact] filler — "+strings.Repeat("b", 200)+" (1d ago)\n", 60)+late)
	h := &hits{}
	srv := hookServer(t, false, big, h)

	res := Runner{BaseURL: srv.URL, Timeout: 5 * time.Second}.Run(Case{
		Name: "c", Mode: "hook", Prompt: "p", CWD: "/c", Expect: []string{"late title"},
	})
	if strings.Join(h.paths, ",") != "/v1/retrieve" || h.sids[0] != "imem-eval" {
		t.Fatalf("want one /v1/retrieve call as imem-eval, got %v %v", h.paths, h.sids)
	}
	if res.Score.Hit || res.Chars != len(big) || len(res.Titles) == 0 || res.Titles[0] != "Early title" {
		t.Fatalf("a title past the 2KB preview must not count: %+v", res)
	}
}

func TestRunnerHookPrefersPreview(t *testing.T) {
	h := &hits{}
	srv := hookServer(t, true, block("- [fact] Jago Syariah binds — c (1d ago)"), h)
	res := Runner{BaseURL: srv.URL, Timeout: 5 * time.Second}.Run(Case{
		Name: "c", Mode: "hook", Prompt: "p", Expect: []string{"jago syariah"},
	})
	if strings.Join(h.paths, ",") != "/v1/retrieve/preview" || !res.Score.Hit || res.Score.Rank != 1 {
		t.Fatalf("want a single preview call and a rank-1 hit, got %v %+v", h.paths, res)
	}
}

// Search mode decodes /v1/memories the way `imem search` does, from the
// daemon's real response type, with the agents' limit of 10.
func TestRunnerSearchDecodesScored(t *testing.T) {
	var gotLimit string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/memories", func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_ = json.NewEncoder(w).Encode(map[string]any{"project": "/c", "memories": []retrieve.Scored{
			{Candidate: graph.Candidate{Title: "IFA link v2"}, Score: 9},
			{Candidate: graph.Candidate{Title: "Jago Syariah binds to exactly one Stockbit account"}, Score: 5},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	res := Runner{BaseURL: srv.URL, Timeout: 5 * time.Second}.Run(Case{
		Name: "s", Mode: "search", Prompt: "jago syariah", CWD: "/c", Expect: []string{"jago syariah binds"},
	})
	if gotLimit != "10" || !res.Score.Hit || res.Score.Rank != 2 {
		t.Fatalf("want limit 10 and a rank-2 hit, got limit %q %+v", gotLimit, res)
	}
}

func TestRunnerHookSendsTheCaseContext(t *testing.T) {
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/retrieve/preview", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Context string `json:"context"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = req.Context
		_ = json.NewEncoder(w).Encode(map[string]any{"context": ""})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	Runner{BaseURL: srv.URL, Timeout: 5 * time.Second}.Run(Case{Name: "c", Mode: "hook", Prompt: "lanjut", Context: "imem spec"})
	if got != "imem spec" {
		t.Fatalf("a follow-up case must carry its conversation context, got %q", got)
	}
}
