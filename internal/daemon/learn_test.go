package daemon

import (
	"strings"
	"testing"

	"github.com/Rampo0/infinite-memory/internal/extract"
	"github.com/Rampo0/infinite-memory/internal/graph"
)

func TestLearnedAliasesComeOnlyFromSearchesThatWereUsed(t *testing.T) {
	shown := []injected{
		{ID: "m1", Title: "BCA RDN creation", Content: "Callback rejected when the signature is stale.", Via: viaSearch,
			Query: "callback bca rekening dana nasabah stuck"},
		{ID: "m2", Title: "Hook found it", Content: "x", Via: "hook"},
		{ID: "m3", Title: "Searched but wrong", Content: "y", Via: viaSearch, Query: "something else entirely"},
		{ID: "m4", Title: "Both found it", Content: "z", Via: "hook"},
		{ID: "m4", Title: "Both found it", Content: "z", Via: viaSearch, Query: "brand new words here"},
	}
	verdicts := []graph.Verdict{{ID: "m1", Verdict: "used"}, {ID: "m2", Verdict: "used"}, {ID: "m3", Verdict: "wrong"},
		{ID: "m4", Verdict: "used"}}
	res := learnAliases(shown, verdicts)
	got := res.Aliases
	if res.Used != 3 || res.SearchOnly != 1 {
		t.Fatalf("three memories were used, one of them only a search found: %+v", res)
	}
	if len(got) != 1 || len(got["m1"]) == 0 || len(got["m1"]) > 3 {
		t.Fatalf("only a used memory that a search surfaced learns, at most 3 terms: %v", got)
	}
	for _, a := range got["m1"] {
		if a == "callback" || a == "bca" {
			t.Fatalf("a term the memory already has is not new: %v", got["m1"])
		}
	}
	if !strings.Contains(strings.Join(got["m1"], " "), "rekening") {
		t.Fatalf("the user's own vocabulary is what the hook missed: %v", got["m1"])
	}
}

func TestLearningIsSkippedForBots(t *testing.T) {
	w, st, sp := testWorker(24000)
	_ = st
	var learned *learnResult
	w.Learn = func(r learnResult) { learned = &r }
	w.Shown = func(string, int64) []extract.Known { return nil }
	sp.err = nil
	long := strings.Repeat("worth saving ", 30)
	p := conversation(t, long, long)
	if _, err := w.process(Job{SessionID: "s", TranscriptPath: p, CWD: "/c", Agent: true}); err != nil {
		t.Fatal(err)
	}
	if learned != nil {
		t.Fatalf("a bot transcript never teaches the index: %+v", learned)
	}
}
