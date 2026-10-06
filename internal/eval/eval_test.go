package eval

import (
	"math"
	"strings"
	"testing"
)

func block(lines ...string) string {
	return "<infinite-memory project=\"/p\">\n" +
		"Long-term memories from previous sessions (background knowledge; verify before relying on it):\n" +
		strings.Join(lines, "\n") + "\n</infinite-memory>"
}

// What the model actually sees: Claude Code inlines additionalContext up to
// 10,000 chars; past that it saves the block to a file and shows a 2KB preview.
func TestVisibleTextInlineUnderCap(t *testing.T) {
	b := strings.Repeat("x", 10000)
	if got := VisibleText(b); got != b {
		t.Fatalf("a block at the cap is fully visible, got %d chars", len(got))
	}
}

func TestVisibleTextPreviewOverCap(t *testing.T) {
	b := strings.Repeat("x", 10001)
	if got := VisibleText(b); len(got) != 2048 {
		t.Fatalf("an oversized block shows only the 2KB preview, got %d chars", len(got))
	}
}

func TestTitlesFromBlockStopsAtRules(t *testing.T) {
	b := block(
		"- [fact] Jago Syariah binds to one account — content here (3d ago)",
		"- [decision] Chose Proposal 1: inline screening — more (5d ago, from registration)",
		"Standing rules and conventions (follow these unless the user says otherwise):",
		"- [rule] Never use --bare — content (1d ago)",
	)
	got := TitlesFromBlock(b)
	want := []string{"Jago Syariah binds to one account", "Chose Proposal 1: inline screening"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestScoreRanksFirstExpectedTitle(t *testing.T) {
	titles := []string{"IFA link v2 changed status gate", "Jago Syariah binds to exactly one Stockbit account", "x"}
	r := Score([]string{"jago syariah binds", "never present"}, titles)
	if !r.Hit || r.Rank != 2 || r.Recall != 0.5 {
		t.Fatalf("want hit at rank 2 with recall 0.5, got %+v", r)
	}
}

func TestScoreMiss(t *testing.T) {
	r := Score([]string{"jago"}, []string{"argocd overlays", "kafka consumers"})
	if r.Hit || r.Rank != 0 || r.Recall != 0 {
		t.Fatalf("want a clean miss, got %+v", r)
	}
}

func TestSummarize(t *testing.T) {
	s := Summarize([]Result{
		{Mode: "hook", Score: CaseScore{Hit: true, Rank: 1, Recall: 1}, Chars: 4000, MS: 10},
		{Mode: "hook", Score: CaseScore{Hit: true, Rank: 4, Recall: 0.5}, Chars: 500000, MS: 30000},
		{Mode: "hook", Score: CaseScore{}, Chars: 0, MS: 20},
	})
	h := s["hook"]
	if h.N != 3 || math.Abs(h.HitRate-2.0/3) > 1e-9 || math.Abs(h.MRR-(1+0.25)/3) > 1e-9 {
		t.Fatalf("hit rate / MRR wrong: %+v", h)
	}
	if math.Abs(h.MeanRecall-0.5) > 1e-9 || h.MaxChars != 500000 || h.P50MS != 20 || h.MaxMS != 30000 {
		t.Fatalf("recall / chars / latency wrong: %+v", h)
	}
}

func TestParseCasesSkipsBlankAndComments(t *testing.T) {
	in := "# interactive\n{\"name\":\"a\",\"mode\":\"hook\",\"prompt\":\"p\",\"cwd\":\"/c\",\"expect\":[\"x\"]}\n\n" +
		"{\"name\":\"b\",\"mode\":\"search\",\"prompt\":\"q\",\"cwd\":\"/c\",\"expect\":[\"y\"]}\n"
	cases, err := ParseCases(strings.NewReader(in))
	if err != nil || len(cases) != 2 || cases[1].Mode != "search" || cases[0].Expect[0] != "x" {
		t.Fatalf("got %+v, %v", cases, err)
	}
}

func TestParseCasesRejectsUnknownMode(t *testing.T) {
	if _, err := ParseCases(strings.NewReader(`{"name":"a","mode":"magic","prompt":"p","expect":["x"]}`)); err == nil {
		t.Fatal("an unknown mode must be an error, not a silently skipped case")
	}
}
