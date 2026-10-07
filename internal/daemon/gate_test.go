package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/retrieve"
)

func TestGateFiresOncePerPromptAndNeverAfterASearch(t *testing.T) {
	g := newGateLog()
	if !g.Check("s", "u1", false) {
		t.Fatal("a turn that never searched must fire")
	}
	if g.Check("s", "u1", false) {
		t.Fatal("the gate fires at most once per prompt")
	}
	if g.Check("s", "u2", true) {
		t.Fatal("a turn that searched must pass")
	}
	if g.Check("s", "", false) {
		t.Fatal("an unknown prompt must pass")
	}
}

func TestGateStatsCountPromptsNotStops(t *testing.T) {
	g := newGateLog()
	g.Check("s", "u1", false)
	g.Check("s", "u1", true)
	g.Check("s", "u2", true)
	g.Check("s", "u2", true)
	st := g.Stats(0)
	if st.Prompts != 2 || st.Searched != 2 || st.Fired != 1 {
		t.Fatalf("want 2 prompts, both searched in the end, gate fired once: %+v", st)
	}
}

func TestGateEndpoint(t *testing.T) {
	s := &server{gates: newGateLog()}
	body, _ := json.Marshal(map[string]any{"session_id": "s", "prompt_id": "u1", "searched": false})
	w := httptest.NewRecorder()
	s.handleGate(w, httptest.NewRequest(http.MethodPost, "/v1/gate", bytes.NewReader(body)))
	var out map[string]any
	_ = json.NewDecoder(w.Body).Decode(&out)
	if out["fire"] != true {
		t.Fatalf("got %v", out)
	}
}

func TestPageOfSkipsTheOffset(t *testing.T) {
	all := []retrieve.Scored{{Candidate: graph.Candidate{ID: "a"}}, {Candidate: graph.Candidate{ID: "b"}}, {Candidate: graph.Candidate{ID: "c"}}}
	if got := pageOf(all, 2); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("offset 2 must start at the third match, got %+v", got)
	}
	if got := pageOf(all, 5); got == nil || len(got) != 0 {
		t.Fatalf("an offset past the end is an empty page, got %+v", got)
	}
}
