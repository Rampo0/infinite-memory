package retrieve

import (
	"strings"
	"testing"

	"github.com/Rampo0/infinite-memory/internal/graph"
)

func cand(id string, lastSeen, seen, hits int64) graph.Candidate {
	return graph.Candidate{
		ID: id, Title: "title-" + id, Content: "content-" + id, Kind: "fact",
		LastSeen: lastSeen, SeenCount: seen, Hits: hits,
	}
}

func TestMergeAndScoreEntityBeatsKeyword(t *testing.T) {
	now := int64(1_000_000)
	q1 := []graph.Candidate{cand("kw", now, 1, 1)}
	q2 := []graph.Candidate{cand("ent", now, 1, 1)}
	out := MergeAndScore(q1, q2, nil, now)
	if len(out) != 2 {
		t.Fatalf("want 2 results, got %d", len(out))
	}
	if out[0].ID != "ent" {
		t.Fatalf("entity hit should outrank keyword hit, got %q first", out[0].ID)
	}
}

func TestMergeAndScoreMergesSameID(t *testing.T) {
	now := int64(1_000_000)
	q1 := []graph.Candidate{cand("m", now, 1, 2)}
	q2 := []graph.Candidate{cand("m", now, 1, 1)}
	q3 := []graph.Candidate{cand("m", now, 1, 10)} // capped at 4
	out := MergeAndScore(q1, q2, q3, now)
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
	old := cand("old", now-30*86400, 1, 1)
	fresh := cand("fresh", now-3600, 1, 1)
	out := MergeAndScore([]graph.Candidate{old, fresh}, nil, nil, now)
	if out[0].ID != "fresh" {
		t.Fatalf("fresher memory should rank first, got %q", out[0].ID)
	}
}

func TestFormatBlock(t *testing.T) {
	now := int64(1_000_000)
	mems := []Scored{{
		Candidate: graph.Candidate{
			ID: "x", Title: "Daemon port", Kind: "decision",
			Content:  strings.Repeat("z", 50),
			LastSeen: now - 3*86400,
		},
	}}
	block := FormatBlock("/proj", mems, 10, now)
	for _, want := range []string{
		`<infinite-memory project="/proj">`,
		"[decision] Daemon port",
		"zzzzzzzzzz…", // clipped to 10 chars
		"(3d ago)",
		"</infinite-memory>",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block missing %q:\n%s", want, block)
		}
	}
}
