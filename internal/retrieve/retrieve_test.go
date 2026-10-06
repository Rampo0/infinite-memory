package retrieve

import (
	"fmt"
	"math"
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
	out := MergeAndScore(q1, q2, nil, now, "/p", ScoreOpts{})
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
	q3 := []graph.Candidate{cand("m", "/p", now, 1, 10)}                              // capped at 4
	out := MergeAndScore(q1, q2, q3, now, "/other", ScoreOpts{SameProjectBoost: 5.0}) // boost must NOT apply
	if len(out) != 1 {
		t.Fatalf("want 1 merged result, got %d", len(out))
	}
	// match = 1*2 + 2*1 + 0.25*4 = 5; recency = 2.0; seen = 0.3*ln(2)
	if out[0].Score < 7.0 || out[0].Score > 7.3 {
		t.Fatalf("unexpected merged score %.3f", out[0].Score)
	}
}

func TestMergeAndScoreRecencyBoost(t *testing.T) {
	now := int64(2_000_000)
	old := cand("old", "/p", now-30*86400, 1, 1)
	fresh := cand("fresh", "/p", now-3600, 1, 1)
	out := MergeAndScore([]graph.Candidate{old, fresh}, nil, nil, now, "/p", ScoreOpts{})
	if out[0].ID != "fresh" {
		t.Fatalf("fresher memory should rank first, got %q", out[0].ID)
	}
}

func TestMergeAndScoreSameProjectBoost(t *testing.T) {
	now := int64(1_000_000)
	local := cand("local", "/here", now, 1, 1)
	foreign := cand("foreign", "/there", now, 1, 1)
	out := MergeAndScore([]graph.Candidate{foreign, local}, nil, nil, now, "/here", ScoreOpts{SameProjectBoost: 1.0})
	if out[0].ID != "local" {
		t.Fatalf("same-project memory should win ties, got %q first", out[0].ID)
	}
	// Strong foreign topic match must still beat weak local match.
	strongForeign := cand("strong", "/there", now, 1, 1)
	weakLocal := cand("weak", "/here", now, 1, 1)
	out = MergeAndScore([]graph.Candidate{weakLocal}, []graph.Candidate{strongForeign}, nil, now, "/here", ScoreOpts{SameProjectBoost: 1.0})
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

// Expand is the only new behaviour on this path: nil must be a perfect no-op
// (every other test in this file is the regression proof), and a non-nil
// Expand must reach the token list the queries are built from.
func TestQueryExpandHook(t *testing.T) {
	var seen []string
	r := Retriever{Expand: func(tokens []string) []string {
		seen = append([]string(nil), tokens...)
		return append(tokens, "guard insert remisier")
	}}
	got := r.expandTokens("solve this issue")

	if len(seen) != 1 || seen[0] != "solve" {
		t.Fatalf("Expand received %v, want the prompt's own tokens", seen)
	}
	if len(got) != 2 || got[1] != "guard insert remisier" {
		t.Fatalf("got %v, want the expanded term appended", got)
	}
}

// A prompt made entirely of stopwords tokenizes to nothing. Rescuing exactly
// that case is the point, so expansion must run before the empty check.
func TestQueryExpandRunsOnEmptyTokens(t *testing.T) {
	called := false
	r := Retriever{Expand: func(tokens []string) []string {
		called = true
		return append(tokens, "rdn creation")
	}}
	got := r.expandTokens("the it is of")
	if !called {
		t.Fatal("Expand must run even when the prompt tokenizes to nothing")
	}
	if len(got) != 1 || got[0] != "rdn creation" {
		t.Fatalf("got %v, want the expansion to stand alone", got)
	}
}

func TestQueryExpandNilIsNoOp(t *testing.T) {
	r := Retriever{}
	if got := r.expandTokens("solve this issue"); len(got) != 1 || got[0] != "solve" {
		t.Fatalf("got %v, want the bare tokens", got)
	}
}

func matched(c graph.Candidate, terms ...string) graph.Candidate {
	c.Matched = terms
	c.Hits = int64(len(terms))
	return c
}

// fillers adds n other memories that also match term, raising its df.
func fillers(n int, term string, now int64) []graph.Candidate {
	out := make([]graph.Candidate, n)
	for i := range out {
		out[i] = matched(cand(fmt.Sprintf("f%03d", i), "/other", now-90*86400, 1, 0), term)
	}
	return out
}

// "account" sits in a quarter of the graph; "syariah" in one memory. One rare
// hit must outrank one common hit, whatever recency and project say.
func TestIDFRareTokenBeatsCommonToken(t *testing.T) {
	now := int64(10_000_000)
	rare := matched(cand("rare", "/there", now-60*86400, 1, 0), "syariah")
	common := matched(cand("common", "/here", now, 1, 0), "account")
	q1 := append([]graph.Candidate{rare, common}, fillers(500, "account", now)...)
	out := MergeAndScore(q1, nil, nil, now, "/here", ScoreOpts{SameProjectBoost: 1, Corpus: 2000})
	if out[0].ID != "rare" {
		t.Fatalf("rare term should rank first, got %q (%.2f) over rare", out[0].ID, out[0].Score)
	}
}

func TestIDFAppliesToEntities(t *testing.T) {
	now := int64(10_000_000)
	specific := matched(cand("specific", "/p", now, 1, 0), "jago syariah")
	generic := matched(cand("generic", "/p", now, 1, 0), "account")
	q2 := append([]graph.Candidate{generic, specific}, fillers(400, "account", now)...)
	out := MergeAndScore(nil, q2, nil, now, "/p", ScoreOpts{Corpus: 2000})
	if out[0].ID != "specific" {
		t.Fatalf("a rare entity should outrank a common one, got %q", out[0].ID)
	}
}

// 1-hop RELATED expansion was the amplifier (1020 of 1133 injected memories
// for one prompt): it may only re-rank what keywords or entities found.
func TestRelatedIsBonusOnly(t *testing.T) {
	now := int64(10_000_000)
	direct := matched(cand("direct", "/p", now, 1, 0), "syariah")
	q3 := []graph.Candidate{cand("direct", "/p", now, 1, 2), cand("neighbour-only", "/p", now, 1, 4)}
	out := MergeAndScore([]graph.Candidate{direct}, nil, q3, now, "/p", ScoreOpts{Corpus: 2000})
	if len(out) != 1 || out[0].ID != "direct" {
		t.Fatalf("a RELATED-only memory must not be a candidate, got %+v", out)
	}
	alone := MergeAndScore([]graph.Candidate{direct}, nil, nil, now, "/p", ScoreOpts{Corpus: 2000})
	if out[0].Score <= alone[0].Score {
		t.Fatal("RELATED hits should still add a bonus to a direct match")
	}
}

// The floor judges relevance alone: recency and the same-project boost are
// added after it, so they cannot rescue a memory that only shares filler.
func TestMinMatchFloorIgnoresBoosts(t *testing.T) {
	now := int64(10_000_000)
	weak := matched(cand("weak", "/here", now, 1, 0), "account")
	strong := matched(cand("strong", "/there", now-60*86400, 1, 0), "syariah")
	q1 := append([]graph.Candidate{weak, strong}, fillers(500, "account", now)...)
	out := MergeAndScore(q1, nil, nil, now, "/here", ScoreOpts{SameProjectBoost: 1, Corpus: 2000, MinMatch: 2})
	for _, s := range out {
		if s.ID == "weak" {
			t.Fatalf("a lone common term (idf < 2) must fall under the floor even fresh and local")
		}
	}
	if len(out) == 0 || out[0].ID != "strong" {
		t.Fatalf("the rare match must survive the floor, got %+v", out)
	}
	if all := MergeAndScore(q1, nil, nil, now, "/here", ScoreOpts{Corpus: 2000}); len(all) != 502 {
		t.Fatalf("MinMatch 0 keeps every candidate (search path), got %d", len(all))
	}
}

// Without a corpus size (or matched lists) every hit weighs 1, as before.
func TestNoCorpusKeepsHitCounts(t *testing.T) {
	now := int64(1_000_000)
	q1 := []graph.Candidate{cand("a", "/p", now, 1, 3)}
	out := MergeAndScore(q1, nil, nil, now, "/p", ScoreOpts{})
	// match = 3; recency = 2.0; seen = 0.3*ln(2)
	if out[0].Score < 5.1 || out[0].Score > 5.3 {
		t.Fatalf("unexpected score %.3f", out[0].Score)
	}
}

// Indonesian filler used to eat the 24-token budget before the topic words
// ("infinite" was cut). With stopwords handled, 32 slots carry the topic.
func TestPromptTokenCapIs32(t *testing.T) {
	words := make([]string, 40)
	for i := range words {
		words[i] = fmt.Sprintf("topic%02d", i)
	}
	r := Retriever{}
	got := r.expandTokens(strings.Join(words, " "))
	if len(got) != 32 || got[31] != "topic31" {
		t.Fatalf("want the first 32 tokens, got %d: %v", len(got), got)
	}
}

func manyMems(n, contentLen int, now int64) []Scored {
	out := make([]Scored, n)
	for i := range out {
		out[i] = Scored{Candidate: graph.Candidate{ID: fmt.Sprintf("m%02d", i), Title: fmt.Sprintf("Memory %02d", i),
			Kind: "fact", ProjectKey: "/p", Content: strings.Repeat("c", contentLen), LastSeen: now}, Score: float64(100 - i)}
	}
	return out
}

// Claude Code inlines additionalContext only up to 10,000 chars; past that the
// model sees a 2KB preview. The block must fit its budget, keeping the best.
func TestBuildBlockStaysUnderBudget(t *testing.T) {
	now := int64(1_000_000)
	mems := manyMems(30, 300, now)
	block, shown, shownRules := BuildBlock("/p", mems, nil, 400, 3000, now)
	if len(block) > 3000 {
		t.Fatalf("block is %d chars, budget 3000", len(block))
	}
	if len(shown) == 0 || len(shown) >= 30 || len(shownRules) != 0 {
		t.Fatalf("want a strict, non-empty prefix of the memories, got %d", len(shown))
	}
	for i, m := range shown {
		if m.ID != mems[i].ID {
			t.Fatalf("shown must be the top-scored prefix, position %d is %s", i, m.ID)
		}
	}
	if want := fmt.Sprintf("… %d more matched memories not shown — imem_search finds more", 30-len(shown)); !strings.Contains(block, want) {
		t.Fatalf("block should say %q:\n%s", want, block)
	}
	if !strings.HasSuffix(block, "</infinite-memory>") {
		t.Fatal("block must stay well-formed")
	}
}

func TestBuildBlockNoBudget(t *testing.T) {
	now := int64(1_000_000)
	block, shown, _ := BuildBlock("/p", manyMems(30, 300, now), nil, 400, 0, now)
	if len(shown) != 30 || strings.Contains(block, "not shown") {
		t.Fatalf("no budget keeps everything, got %d shown", len(shown))
	}
}

// Rules (when they ride along per prompt) take what the memories leave.
func TestBuildBlockRulesFillTheRemainder(t *testing.T) {
	now := int64(1_000_000)
	rules := []graph.Candidate{
		{ID: "r1", Title: "Max args", Kind: "rule", ProjectKey: "/p", Content: strings.Repeat("r", 300), LastSeen: now},
		{ID: "r2", Title: "LOC limit", Kind: "rule", ProjectKey: "/p", Content: strings.Repeat("r", 300), LastSeen: now},
	}
	block, shown, shownRules := BuildBlock("/p", manyMems(2, 300, now), rules, 400, 1450, now)
	if len(block) > 1450 || len(shown) != 2 || len(shownRules) != 1 {
		t.Fatalf("want both memories and one rule in 1450 chars, got %d mems %d rules (%d chars)", len(shown), len(shownRules), len(block))
	}
	if !strings.Contains(block, "… 1 more standing rules not shown") {
		t.Fatalf("omitted rules must be counted:\n%s", block)
	}
}

// Memories already injected earlier in the session are still in the model's
// context: re-sending them only spends the budget on duplicates.
func TestWithoutIDs(t *testing.T) {
	mems := manyMems(4, 10, 1)
	got := withoutIDs(mems, map[string]bool{"m01": true, "m03": true})
	if len(got) != 2 || got[0].ID != "m00" || got[1].ID != "m02" {
		t.Fatalf("got %+v", got)
	}
	if len(withoutIDs(mems, nil)) != 4 {
		t.Fatal("nil exclude keeps everything")
	}
}

// SessionStart standing context, filled in this order until the budget runs
// out: this project's rules (a parent workspace counts as this project), its
// preferences, then other projects' rules and preferences — current project
// first, as the always-on rules section always was.
func TestSessionRulesOrder(t *testing.T) {
	rules := []graph.Candidate{
		{ID: "foreign", Kind: "rule", ProjectKey: "/other", SeenCount: 9, LastSeen: 100},
		{ID: "local-old", Kind: "rule", ProjectKey: "/ws/here", SeenCount: 1, LastSeen: 50},
		{ID: "workspace", Kind: "rule", ProjectKey: "/ws", SeenCount: 1, LastSeen: 60},
		{ID: "local-hot", Kind: "rule", ProjectKey: "/ws/here", SeenCount: 5, LastSeen: 90},
		{ID: "sibling", Kind: "rule", ProjectKey: "/ws/here-not", SeenCount: 1, LastSeen: 95},
	}
	prefs := []graph.Candidate{
		{ID: "pref-foreign", Kind: "preference", ProjectKey: "/other", SeenCount: 1, LastSeen: 80},
		{ID: "pref-local", Kind: "preference", ProjectKey: "/ws/here", SeenCount: 1, LastSeen: 10},
	}
	got := SessionRules(rules, prefs, "/ws/here")
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if want := "local-hot,workspace,local-old,pref-local,foreign,sibling,pref-foreign"; strings.Join(ids, ",") != want {
		t.Fatalf("want %s, got %s", want, strings.Join(ids, ","))
	}
}

// imem rules prints exactly the lines the hook block used to carry, because
// ai-review's newest_rules() keeps lines starting with "- [rule]".
func TestRuleLinesMatchBlockLines(t *testing.T) {
	now := int64(1_000_000)
	rules := []graph.Candidate{
		{ID: "r1", Title: "Max args", Kind: "rule", ProjectKey: "/Users/x/accountworkspace/registration", Content: "Max 4 args.", LastSeen: now - 3*86400},
		{ID: "r2", Title: "LOC", Kind: "rule", ProjectKey: "/Users/x/accountworkspace", Content: "Under 60 lines.", LastSeen: now},
	}
	rules = append(rules, graph.Candidate{ID: "r3", Title: "Long", Kind: "rule", ProjectKey: "/Users/x/accountworkspace",
		Content: strings.Repeat("y", 500), LastSeen: now})
	lines := RuleLines("/Users/x/accountworkspace", rules, 400, now)
	block := FormatBlock("/Users/x/accountworkspace", nil, rules, 400, now)
	for _, l := range lines {
		if !strings.HasPrefix(l, "- [rule] ") || !strings.Contains(block, l+"\n") {
			t.Fatalf("line %q must be byte-identical to the block's", l)
		}
	}
	if lines[0] != "- [rule] Max args — Max 4 args. (3d ago, from registration)" {
		t.Fatalf("unexpected line format %q", lines[0])
	}
}

func partial(c graph.Candidate, toks ...string) graph.Candidate {
	c.Partial = toks
	c.Hits = int64(len(toks))
	return c
}

// "imem" is only a token of rare entities like "imem.save": matching it says
// as little as matching the word "imem". A partial entity match weighs the
// matched token (x1), not the rare entity (x2) — that inflation buried the
// memories the prompt was really about.
func TestPartialEntityMatchWeighsAsItsToken(t *testing.T) {
	now := int64(10_000_000)
	var q2 []graph.Candidate
	for i := 0; i < 120; i++ {
		q2 = append(q2, partial(cand(fmt.Sprintf("imem%03d", i), "/p", now, 1, 0), "imem"))
	}
	topic := matched(cand("topic", "/p", now, 1, 0), "expansion")
	q1 := append([]graph.Candidate{topic}, fillers(2, "expansion", now)...)
	out := MergeAndScore(q1, q2, nil, now, "/p", ScoreOpts{Corpus: 2000})
	if out[0].ID != "topic" && out[0].ID != "f000" && out[0].ID != "f001" {
		t.Fatalf("a rare keyword must beat 120 partial matches on a common token, got %q first", out[0].ID)
	}
}

// A partial token that the keyword query already counted is not counted again.
func TestPartialTokenNotDoubleCounted(t *testing.T) {
	now := int64(10_000_000)
	kw := matched(cand("m", "/p", now, 1, 0), "expansion")
	ent := partial(cand("m", "/p", now, 1, 0), "expansion")
	both := MergeAndScore([]graph.Candidate{kw}, []graph.Candidate{ent}, nil, now, "/p", ScoreOpts{Corpus: 2000})
	only := MergeAndScore([]graph.Candidate{kw}, nil, nil, now, "/p", ScoreOpts{Corpus: 2000})
	if math.Abs(both[0].Score-only[0].Score) > 1e-9 {
		t.Fatalf("partial match on an already-counted keyword must add nothing: %.3f vs %.3f", both[0].Score, only[0].Score)
	}
}

// An entity literally named "account" is tagged on 40 memories, but the word
// is in 459: an exact single-word entity match repeats the keyword's evidence
// and must not outweigh a rarer term ("jago").
func TestSingleWordEntityCountsLikeAKeyword(t *testing.T) {
	now := int64(10_000_000)
	acct := cand("acct", "/p", now, 1, 0)
	q1 := []graph.Candidate{matched(acct, "account"), matched(cand("jago", "/p", now, 1, 0), "jago")}
	q1 = append(q1, fillers(458, "account", now)...)
	for i := 0; i < 39; i++ {
		q1 = append(q1, matched(cand(fmt.Sprintf("j%02d", i), "/q", now-90*86400, 1, 0), "jago"))
	}
	q2 := []graph.Candidate{matched(acct, "account")}
	for i := 0; i < 39; i++ {
		q2 = append(q2, matched(cand(fmt.Sprintf("e%02d", i), "/q", now-90*86400, 1, 0), "account"))
	}
	out := MergeAndScore(q1, q2, nil, now, "/p", ScoreOpts{Corpus: 2000})
	if out[0].ID != "jago" {
		t.Fatalf("the rarer word must win over a common word tagged as an entity, got %q first", out[0].ID)
	}
}

// A specific (multi-part) entity name matched exactly is the strongest
// evidence there is and keeps its double weight.
func TestSpecificEntityNameKeepsDoubleWeight(t *testing.T) {
	now := int64(10_000_000)
	specific := matched(cand("spec", "/p", now, 1, 0), "opening-account")
	kw := matched(cand("kw", "/p", now, 1, 0), "zqrare")
	out := MergeAndScore([]graph.Candidate{kw}, []graph.Candidate{specific}, nil, now, "/p", ScoreOpts{Corpus: 2000})
	if out[0].ID != "spec" || out[0].Score < out[1].Score+5 {
		t.Fatalf("an exact specific entity (df 1) should be worth twice a df-1 keyword, got %+v", out)
	}
}

// RELATED co-occurrence is a weak signal: its bonus is capped at 1.0, below
// one medium-rare term.
func TestRelatedBonusIsCapped(t *testing.T) {
	now := int64(10_000_000)
	a := matched(cand("a", "/p", now, 1, 0), "alpha")
	b := matched(cand("b", "/p", now, 1, 0), "alpha", "zqx")
	q1 := append([]graph.Candidate{a, b}, fillers(300, "zqx", now)...)
	q3 := []graph.Candidate{cand("a", "/p", now, 1, 4)}
	out := MergeAndScore(q1, nil, q3, now, "/p", ScoreOpts{Corpus: 2000})
	if out[0].ID != "b" {
		t.Fatalf("four RELATED hits must not beat one more matched term (idf ~1.9), got %q first", out[0].ID)
	}
}

// B2: what the extractor saw being used rises; what it saw disputed sinks;
// and a memory used yesterday is as fresh as one seen yesterday.
func TestFeedbackShapesTheScore(t *testing.T) {
	now := int64(10_000_000)
	base := func(id string) graph.Candidate { return matched(cand(id, "/p", now-60*86400, 1, 0), "syariah") }
	used, plain, disputed, recent := base("used"), base("plain"), base("disputed"), base("recent")
	used.UsedCount = 3
	disputed.DisputedCount = 2
	recent.LastUsed = now - 3600
	out := MergeAndScore([]graph.Candidate{plain, used, disputed, recent}, nil, nil, now, "/p", ScoreOpts{Corpus: 2000})
	var ids []string
	for _, s := range out {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "recent,used,plain,disputed" {
		t.Fatalf("want recent use > used > plain > disputed, got %v", ids)
	}
}

func TestFormatSavedMarksUpdates(t *testing.T) {
	out := FormatSaved([]SavedLine{{Title: "Expansion moved to index time", Kind: "decision", New: true, Updated: true}}, -1)
	if !strings.HasSuffix(out, "replaces older") {
		t.Fatalf("an update must say it replaced an older memory: %q", out)
	}
}
