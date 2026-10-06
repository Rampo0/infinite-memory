package extract

import (
	"github.com/Rampo0/infinite-memory/internal/graph"
	"strings"
	"testing"
)

func TestParseMemoriesFenced(t *testing.T) {
	raw := "```json\n{\"memories\":[{\"title\":\"Port choice\",\"content\":\"Daemon uses 7690.\",\"type\":\"decision\",\"entities\":[{\"name\":\"imem daemon\",\"type\":\"component\"}],\"relations\":[[\"imem daemon\",\"uses\",\"memgraph\"]]}]}\n```"
	mems, err := ParseMemories(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 || mems[0].Kind != "decision" || len(mems[0].Entities) != 1 || len(mems[0].Relations) != 1 {
		t.Fatalf("unexpected parse: %+v", mems)
	}
}

func TestParseMemoriesProseWrapped(t *testing.T) {
	raw := "Here is the extraction:\n{\"memories\":[{\"title\":\"t\",\"content\":\"c\",\"type\":\"fact\"}]}\nDone."
	mems, err := ParseMemories(raw)
	if err != nil || len(mems) != 1 {
		t.Fatalf("mems=%v err=%v", mems, err)
	}
}

func TestParseMemoriesValidation(t *testing.T) {
	raw := `{"memories":[
		{"title":"","content":"skipped: empty title","type":"fact"},
		{"title":"bad kind","content":"c","type":"wisdom"},
		{"title":"bad relation","content":"c","type":"fact","relations":[["a","uses"],["a","uses","b"]]},
		{"title":"long","content":"` + strings.Repeat("x", 2000) + `","type":"fact"}
	]}`
	mems, err := ParseMemories(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 3 {
		t.Fatalf("want 3 valid memories, got %d", len(mems))
	}
	if mems[0].Kind != "fact" {
		t.Fatalf("invalid kind should default to fact, got %q", mems[0].Kind)
	}
	if len(mems[1].Relations) != 1 {
		t.Fatalf("malformed relation should be dropped, got %v", mems[1].Relations)
	}
	if len(mems[2].Content) != maxContentChars {
		t.Fatalf("content not clipped: %d", len(mems[2].Content))
	}
}

func TestParseMemoriesGarbage(t *testing.T) {
	if _, err := ParseMemories("total nonsense, no braces"); err == nil {
		t.Fatal("want error on garbage")
	}
}

func TestParseMemoriesEmptyList(t *testing.T) {
	mems, err := ParseMemories(`{"memories":[]}`)
	if err != nil || len(mems) != 0 {
		t.Fatalf("empty list should parse cleanly: mems=%v err=%v", mems, err)
	}
}

// Aliases are index-time query expansion: the words a future prompt would
// use for this memory (synonyms, Indonesian, joined identifiers). Bounded,
// because the extractor reads untrusted text and a stuffed alias list would
// make one memory match everything.
func TestParseMemoriesAliases(t *testing.T) {
	long := strings.Repeat("x", 41)
	raw := `{"memories":[{"title":"t","content":"c","type":"fact","aliases":[` +
		`" BCARDN ","bca rdn","bcardn","","` + long + `","rekening dana nasabah",` +
		`"a1","a2","a3","a4","a5","a6","a7","a8"]}]}`
	mems, err := ParseMemories(raw)
	if err != nil || len(mems) != 1 {
		t.Fatalf("mems=%v err=%v", mems, err)
	}
	got := mems[0].Aliases
	if len(got) != 10 || got[0] != "bcardn" || got[1] != "bca rdn" || got[2] != "rekening dana nasabah" {
		t.Fatalf("want ≤10 trimmed, lowercased, deduped aliases without blanks or 40+ chars, got %q", got)
	}
}

func TestParseMemoriesWithoutAliases(t *testing.T) {
	mems, err := ParseMemories(`{"memories":[{"title":"t","content":"c","type":"fact"}]}`)
	if err != nil || len(mems) != 1 || len(mems[0].Aliases) != 0 {
		t.Fatalf("aliases are optional: %+v %v", mems, err)
	}
}

// Reconcile (B1): each memory says whether it is new, replaces an existing
// memory, or restates one; feedback (B2) grades memories the assistant was
// shown.
func TestParseExtractionOpsAndFeedback(t *testing.T) {
	raw := `{"memories":[` +
		`{"title":"a","content":"c","type":"fact"},` +
		`{"title":"b","content":"c","type":"fact","op":"update","target_id":"id1"},` +
		`{"title":"c","content":"c","type":"fact","op":"NOOP","target_id":"id2"},` +
		`{"title":"d","content":"c","type":"fact","op":"merge","target_id":"id3"}],` +
		`"feedback":[{"id":"s1","verdict":"used"},{"id":"s2","verdict":"Wrong"},{"id":"s3","verdict":"meh"},{"id":"","verdict":"used"}]}`
	mems, fb, err := ParseExtraction(raw)
	if err != nil {
		t.Fatal(err)
	}
	ops := []string{}
	for _, m := range mems {
		ops = append(ops, m.Op+":"+m.TargetID)
	}
	if strings.Join(ops, ",") != "add:,update:id1,noop:id2,add:" {
		t.Fatalf("ops normalized wrong: %v", ops)
	}
	if len(fb) != 2 || fb[0] != (Feedback{ID: "s1", Verdict: "used"}) || fb[1] != (Feedback{ID: "s2", Verdict: "wrong"}) {
		t.Fatalf("feedback must keep valid verdicts only: %+v", fb)
	}
}

// update/noop may only name a memory the extractor was shown; anything else
// falls back to a plain add (update) or is dropped (noop: it claimed the
// fact was known, but cannot say where).
func TestResolveOpsWhitelist(t *testing.T) {
	mems := []graph.MemoryIn{
		{Title: "u-ok", Op: "update", TargetID: "known"},
		{Title: "u-bad", Op: "update", TargetID: "invented"},
		{Title: "n-ok", Op: "noop", TargetID: "known"},
		{Title: "n-bad", Op: "noop", TargetID: "invented"},
		{Title: "a", Op: "add"},
	}
	got := ResolveOps(mems, map[string]bool{"known": true})
	var out []string
	for _, m := range got {
		out = append(out, m.Title+"="+m.Op+":"+m.TargetID)
	}
	if strings.Join(out, ",") != "u-ok=update:known,u-bad=add:,n-ok=noop:known,a=add:" {
		t.Fatalf("got %v", out)
	}
}
