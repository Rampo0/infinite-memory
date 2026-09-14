package retrieve

import (
	"fmt"
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
	out := MergeAndScore(q1, q2, q3, now, "/other", 5.0) // boost must NOT apply
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

// k <= 0 means no cap: rules_k -1 injects every standing rule.
func TestSortRulesNoLimit(t *testing.T) {
	rules := []graph.Candidate{
		{ID: "foreign", Kind: "rule", ProjectKey: "/other", SeenCount: 9, LastSeen: 100},
		{ID: "local-old", Kind: "rule", ProjectKey: "/here", SeenCount: 1, LastSeen: 50},
		{ID: "local-hot", Kind: "rule", ProjectKey: "/here", SeenCount: 5, LastSeen: 90},
	}
	for _, k := range []int{-1, -50} {
		out := SortRules(rules, "/here", k, nil)
		if len(out) != 3 {
			t.Fatalf("k=%d must not cap, got %d rules", k, len(out))
		}
		if out[0].ID != "local-hot" || out[2].ID != "foreign" {
			t.Fatalf("k=%d lost the ordering: %+v", k, out)
		}
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

func summaryFixture(now int64) ([]Scored, []graph.Candidate) {
	mems := []Scored{{
		Candidate: graph.Candidate{
			ID: "a", Title: "Recursion guard via env sentinel", Kind: "fact",
			ProjectKey: "/proj", LastSeen: now - 7*86400,
		},
		Score: 8.42,
	}, {
		Candidate: graph.Candidate{
			ID: "b", Title: "Field masks modeled as a domain enum, not proto types", Kind: "decision",
			ProjectKey: "/Users/x/accountworkspace/opening-account", LastSeen: now - 3*3600,
		},
		Score: 7.05,
	}, {
		Candidate: graph.Candidate{
			ID: "c", Title: "Third memory", Kind: "preference", ProjectKey: "/proj", LastSeen: now,
		},
		Score: 6.9,
	}}
	rules := []graph.Candidate{
		{ID: "r1", Kind: "rule", Title: "Max args", ProjectKey: "/proj", LastSeen: now},
		{ID: "r2", Kind: "rule", Title: "No bare worktrees", ProjectKey: "/proj", LastSeen: now},
	}
	return mems, rules
}

func TestFormatSummary(t *testing.T) {
	now := int64(1_000_000_000)
	mems, rules := summaryFixture(now)
	got := FormatSummary("/proj", mems, rules, 6, now)
	for _, want := range []string{
		"[fact]",
		"Recursion guard via env sentinel",
		"7d ago",
		"8.4", // score, one decimal
		"[decision]",
		"Field masks modeled as a domain enum, not proto…", // clipped to 48 runes
		"↖opening-account", // foreign project marker
		"[preference]",
		"2 standing rules",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "more") {
		t.Fatalf("nothing was dropped, want no \"… N more\":\n%s", got)
	}
	if n := len(strings.Split(got, "\n")); n != 4 {
		t.Fatalf("want 3 memory lines + tail, got %d lines:\n%s", n, got)
	}
	if strings.Contains(got, "↖proj") {
		t.Fatalf("same-project memory must not be marked:\n%s", got)
	}
	if strings.HasSuffix(got, "\n") || strings.Contains(got, " \n") {
		t.Fatalf("summary has trailing whitespace:\n%q", got)
	}
}

func TestFormatSummaryCapsLines(t *testing.T) {
	now := int64(1_000_000_000)
	mems, rules := summaryFixture(now)
	got := FormatSummary("/proj", mems, rules, 1, now)
	if !strings.Contains(got, "… 2 more · 2 standing rules") {
		t.Fatalf("want capped tail line:\n%s", got)
	}
	if strings.Contains(got, "Third memory") {
		t.Fatalf("line beyond the cap leaked in:\n%s", got)
	}
	if n := len(strings.Split(got, "\n")); n != 2 {
		t.Fatalf("want 1 memory line + tail, got %d:\n%s", n, got)
	}
}

// hook_summary_lines -1 lists every memory, so no "… N more" tail.
func TestFormatSummaryNoLimit(t *testing.T) {
	now := int64(1_000_000_000)
	mems, rules := summaryFixture(now)
	got := FormatSummary("/proj", mems, rules, -1, now)
	if strings.Contains(got, "more") {
		t.Fatalf("maxLines -1 must not elide anything:\n%s", got)
	}
	if !strings.Contains(got, "Third memory") {
		t.Fatalf("last memory missing:\n%s", got)
	}
	if n := len(strings.Split(got, "\n")); n != 4 {
		t.Fatalf("want 3 memory lines + tail, got %d:\n%s", n, got)
	}
}

func TestFormatSummaryRulesOnlyAndEmpty(t *testing.T) {
	now := int64(1_000_000_000)
	_, rules := summaryFixture(now)
	if got := FormatSummary("/proj", nil, rules, 6, now); got != "  2 standing rules" {
		t.Fatalf("rules-only summary wrong: %q", got)
	}
	if got := FormatSummary("/proj", nil, nil, 6, now); got != "" {
		t.Fatalf("empty summary must be blank, got %q", got)
	}
}

func TestTruncRunesNeverSplitsRunes(t *testing.T) {
	got := truncRunes("héllo wörld ünicode", 8)
	if got != "héllo w…" {
		t.Fatalf("got %q", got)
	}
	if r := []rune(got); len(r) != 8 {
		t.Fatalf("want 8 runes, got %d in %q", len(r), got)
	}
	if got := truncRunes("short", 48); got != "short" {
		t.Fatalf("short titles must pass through, got %q", got)
	}
}

func savedFixture(n int) []SavedLine {
	out := make([]SavedLine, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, SavedLine{
			Title: fmt.Sprintf("memory number %d", i), Kind: "fact", New: true, Seen: 1,
		})
	}
	return out
}

func TestFormatSavedEmpty(t *testing.T) {
	if got := FormatSaved(nil, -1); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestFormatSaved(t *testing.T) {
	got := FormatSaved([]SavedLine{
		{Title: "Blocking flush at Stop", Kind: "decision", New: true, Seen: 1},
		{Title: "Formatter mirrors FormatSummary", Kind: "rule", New: false, Seen: 4},
	}, -1)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "[decision]") || !strings.HasSuffix(lines[0], "new") {
		t.Fatalf("new line wrong: %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], "seen 4x") {
		t.Fatalf("reseen line should report the count: %q", lines[1])
	}
	for _, l := range lines {
		if l != strings.TrimRight(l, " ") {
			t.Fatalf("trailing whitespace in %q", l)
		}
	}
}

func TestFormatSavedClipsTitle(t *testing.T) {
	long := strings.Repeat("x", summaryTitleChars+20)
	got := FormatSaved([]SavedLine{{Title: long, Kind: "fact", New: true}}, -1)
	if !strings.Contains(got, "…") {
		t.Fatalf("long title should be clipped: %q", got)
	}
	if strings.Contains(got, long) {
		t.Fatal("full title leaked into the line")
	}
}

func TestFormatSavedCapsLines(t *testing.T) {
	got := FormatSaved(savedFixture(5), 2)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 2 lines + tail, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[2], "3 more") {
		t.Fatalf("tail should collapse the rest: %q", lines[2])
	}
}

func TestFormatSavedNoLimit(t *testing.T) {
	got := FormatSaved(savedFixture(5), -1)
	if n := len(strings.Split(got, "\n")); n != 5 {
		t.Fatalf("negative cap means every line, got %d", n)
	}
	if strings.Contains(got, "more") {
		t.Fatal("no tail expected when nothing is capped")
	}
}
