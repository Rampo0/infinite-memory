package retrieve

import (
	"strings"
	"testing"

	"github.com/Rampo0/infinite-memory/internal/graph"
)

func cand(id, pk string, lastSeen, seen, hits int64) graph.Candidate {
	return graph.Candidate{
		ID: id, Title: "title-" + id, Content: "content-" + id, Kind: "fact",
		ProjectKey: pk, LastSeen: lastSeen, SeenCount: seen, Hits: hits,
	}
}

func TestMergeAndScoreEntityBeatsKeyword(t *testing.T) {
	now := int64(1_000_000)
	q1 := []graph.Candidate{cand("kw", "/p", now, 1, 1)}
	q2 := []graph.Candidate{cand("ent", "/p", now, 1, 1)}
	out := MergeAndScore(q1, q2, nil, now, "/p", 0)
	if len(out) != 2 {
		t.Fatalf("want 2 results, got %d", len(out))
	}
	if out[0].ID != "ent" {
		t.Fatalf("entity hit should outrank keyword hit, got %q first", out[0].ID)
	}
}

func TestMergeAndScoreMergesSameID(t *testing.T) {
	now := int64(1_000_000)
	q1 := []graph.Candidate{cand("m", "/p", now, 1, 2)}
	q2 := []graph.Candidate{cand("m", "/p", now, 1, 1)}
	q3 := []graph.Candidate{cand("m", "/p", now, 1, 10)} // capped at 4
	out := MergeAndScore(q1, q2, q3, now, "/other", 5.0)  // boost must NOT apply
	if len(out) != 1 {
		t.Fatalf("want 1 merged result, got %d", len(out))
	}
	// match = 1*2 + 2*1 + 0.75*4 = 7; recency = 2.0; seen = 0.3*ln(2)
	if out[0].Score < 9.0 || out[0].Score > 9.3 {
		t.Fatalf("unexpected merged score %.3f", out[0].Score)
	}
}

func TestMergeAndScoreRecencyBoost(t *testing.T) {
	now := int64(2_000_000)
	old := cand("old", "/p", now-30*86400, 1, 1)
	fresh := cand("fresh", "/p", now-3600, 1, 1)
	out := MergeAndScore([]graph.Candidate{old, fresh}, nil, nil, now, "/p", 0)
	if out[0].ID != "fresh" {
		t.Fatalf("fresher memory should rank first, got %q", out[0].ID)
	}
}

func TestMergeAndScoreSameProjectBoost(t *testing.T) {
	now := int64(1_000_000)
	local := cand("local", "/here", now, 1, 1)
	foreign := cand("foreign", "/there", now, 1, 1)
	out := MergeAndScore([]graph.Candidate{foreign, local}, nil, nil, now, "/here", 1.0)
	if out[0].ID != "local" {
		t.Fatalf("same-project memory should win ties, got %q first", out[0].ID)
	}
	// Strong foreign topic match must still beat weak local match.
	strongForeign := cand("strong", "/there", now, 1, 1)
	weakLocal := cand("weak", "/here", now, 1, 1)
	out = MergeAndScore([]graph.Candidate{weakLocal}, []graph.Candidate{strongForeign}, nil, now, "/here", 1.0)
	if out[0].ID != "strong" {
		t.Fatalf("strong foreign entity match should beat weak local keyword match, got %q", out[0].ID)
	}
}

func TestSortRules(t *testing.T) {
	rules := []graph.Candidate{
		{ID: "foreign", Kind: "rule", ProjectKey: "/other", SeenCount: 9, LastSeen: 100},
		{ID: "local-old", Kind: "rule", ProjectKey: "/here", SeenCount: 1, LastSeen: 50},
		{ID: "local-hot", Kind: "rule", ProjectKey: "/here", SeenCount: 5, LastSeen: 90},
		{ID: "shown", Kind: "rule", ProjectKey: "/here", SeenCount: 99, LastSeen: 99},
	}
	out := SortRules(rules, "/here", 2, map[string]bool{"shown": true})
	if len(out) != 2 || out[0].ID != "local-hot" || out[1].ID != "local-old" {
		t.Fatalf("want [local-hot local-old], got %+v", out)
	}
}

func TestFormatBlock(t *testing.T) {
	now := int64(1_000_000)
	mems := []Scored{{
		Candidate: graph.Candidate{
			ID: "x", Title: "Daemon port", Kind: "decision", ProjectKey: "/proj",
			Content:  strings.Repeat("z", 50),
			LastSeen: now - 3*86400,
		},
	}, {
		Candidate: graph.Candidate{
			ID: "y", Title: "Jago whitelist flow", Kind: "fact",
			ProjectKey: "/Users/x/accountworkspace/opening-account",
			Content:    "whitelist checked via RPC", LastSeen: now - 86400,
		},
	}}
	rules := []graph.Candidate{{
		ID: "r1", Title: "Function length limit", Kind: "rule", ProjectKey: "/proj",
		Content: "Functions stay under 60 lines.", LastSeen: now,
	}}
	block := FormatBlock("/proj", mems, rules, 10, now)
	for _, want := range []string{
		`<infinite-memory project="/proj">`,
		"[decision] Daemon port",
		"zzzzzzzzzz…", // clipped to 10 chars
		"(3d ago)",
		", from opening-account", // foreign memory marked with source project
		"Standing rules and conventions",
		"[rule] Function length limit",
		"</infinite-memory>",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block missing %q:\n%s", want, block)
		}
	}
}

func TestFormatBlockRulesOnly(t *testing.T) {
	rules := []graph.Candidate{{ID: "r1", Title: "Max args", Kind: "rule", ProjectKey: "/p", Content: "Max 4 args.", LastSeen: 1}}
	block := FormatBlock("/p", nil, rules, 100, 2)
	if strings.Contains(block, "Long-term memories") || !strings.Contains(block, "[rule] Max args") {
		t.Fatalf("rules-only block wrong:\n%s", block)
	}
}
