// Package retrieve turns a user prompt into a ranked context block:
// tokenize -> global graph candidate queries -> score in Go (with a
// same-project boost) -> top-K -> plus standing rules -> format.
package retrieve

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/textutil"
)

type Scored struct {
	graph.Candidate
	Score float64 `json:"score"`
	Q1    int64   `json:"q1_hits"`
	Q2    int64   `json:"q2_hits"`
	Q3    int64   `json:"q3_hits"`
}

type Retriever struct {
	Store *graph.Store
	// K caps the scored memories per prompt; K <= 0 means no cap.
	K               int
	MaxContentChars int
	// SameProjectBoost is added to the score of memories from the current
	// project so local context wins ties without hiding other projects.
	SameProjectBoost float64
	// RulesK caps the always-on standing-rules section: 0 disables the
	// section, negative means no cap.
	RulesK int
	// SummaryLines caps the per-memory lines in the CLI summary; the rest
	// collapse into a "… N more" line. Negative means no cap.
	SummaryLines int
	// Expand appends LLM-derived terms to the prompt's own tokens. Nil means
	// no expansion — today's behaviour exactly. It takes no context because
	// the LLM call has already happened by the time Query runs: expansion
	// spends its own budget in the daemon, before the graph deadline starts.
	Expand func(tokens []string) []string
}

// Result is one retrieval: the block injected into the model's context, plus
// the compact summary and counts shown to the user in the CLI.
type Result struct {
	Block    string `json:"block"`
	Summary  string `json:"summary"`
	Memories int    `json:"memories"`
	Rules    int    `json:"rules"`
}

// Query returns the top-K scored memories for a prompt. Matching is global;
// pk only drives the same-project boost.
func (r *Retriever) Query(ctx context.Context, pk, prompt string, now int64) ([]Scored, error) {
	tokens := r.expandTokens(prompt)
	if len(tokens) == 0 {
		return nil, nil
	}
	q1, q2, q3, err := r.Store.Candidates(ctx, tokens)
	if err != nil {
		return nil, err
	}
	scored := MergeAndScore(q1, q2, q3, now, pk, r.SameProjectBoost)
	if r.K > 0 && len(scored) > r.K {
		scored = scored[:r.K]
	}
	return scored, nil
}

// Retrieve returns the context block plus its user-facing summary. Block is ""
// when nothing matched and no rules exist.
func (r *Retriever) Retrieve(ctx context.Context, pk, prompt string, now int64) (Result, error) {
	scored, err := r.Query(ctx, pk, prompt, now)
	if err != nil {
		return Result{}, err
	}

	var rules []graph.Candidate
	if r.RulesK != 0 {
		// Fetch the whole rule pool: the current-project-first ordering and
		// the RulesK cap both happen in SortRules, so a Cypher-side LIMIT
		// would truncate before anything is prioritized.
		all, err := r.Store.Rules(ctx, -1)
		if err != nil {
			return Result{}, err
		}
		seen := make(map[string]bool, len(scored))
		for _, s := range scored {
			seen[s.ID] = true
		}
		rules = SortRules(all, pk, r.RulesK, seen)
	}

	if len(scored) == 0 && len(rules) == 0 {
		return Result{}, nil
	}
	return Result{
		Block:    FormatBlock(pk, scored, rules, r.MaxContentChars, now),
		Summary:  FormatSummary(pk, scored, rules, r.SummaryLines, now),
		Memories: len(scored),
		Rules:    len(rules),
	}, nil
}

// expandTokens is the prompt's own tokens plus whatever the expander added.
// Expansion runs AFTER tokenizing, not before: a prompt made entirely of
// stopwords tokenizes to nothing, and rescuing exactly that case is the point.
func (r *Retriever) expandTokens(prompt string) []string {
	tokens := textutil.Tokenize(prompt, 24)
	if r.Expand == nil {
		return tokens
	}
	return r.Expand(tokens)
}

// MergeAndScore merges the three candidate lists by memory id and ranks:
//
//	match = 1.0*q1 + 2.0*q2 + 0.75*min(q3, 4)   (zero-match discarded)
//	score = match + 2.0*exp(-ageDays/14) + 0.3*ln(1+seen_count)
//	      + sameProjectBoost when the memory belongs to pk
func MergeAndScore(q1, q2, q3 []graph.Candidate, now int64, pk string, sameProjectBoost float64) []Scored {
	byID := map[string]*Scored{}
	absorb := func(cands []graph.Candidate, set func(s *Scored, hits int64)) {
		for _, c := range cands {
			s := byID[c.ID]
			if s == nil {
				s = &Scored{Candidate: c}
				byID[c.ID] = s
			}
			set(s, c.Hits)
		}
	}
	absorb(q1, func(s *Scored, h int64) { s.Q1 = h })
	absorb(q2, func(s *Scored, h int64) { s.Q2 = h })
	absorb(q3, func(s *Scored, h int64) { s.Q3 = h })

	out := make([]Scored, 0, len(byID))
	for _, s := range byID {
		match := 1.0*float64(s.Q1) + 2.0*float64(s.Q2) + 0.75*math.Min(float64(s.Q3), 4)
		if match == 0 {
			continue
		}
		ageDays := float64(now-s.LastSeen) / 86400.0
		if ageDays < 0 {
			ageDays = 0
		}
		s.Score = match + 2.0*math.Exp(-ageDays/14.0) + 0.3*math.Log(1+float64(s.SeenCount))
		if pk != "" && s.ProjectKey == pk {
			s.Score += sameProjectBoost
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// SortRules orders standing rules current-project-first, then by how often
// they were re-observed and how recently, caps at k (k <= 0 means no cap) and
// drops ids already shown in the scored section.
func SortRules(rules []graph.Candidate, pk string, k int, exclude map[string]bool) []graph.Candidate {
	var out []graph.Candidate
	for _, r := range rules {
		if !exclude[r.ID] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := out[i].ProjectKey == pk, out[j].ProjectKey == pk
		if li != lj {
			return li
		}
		if out[i].SeenCount != out[j].SeenCount {
			return out[i].SeenCount > out[j].SeenCount
		}
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen
		}
		return out[i].ID < out[j].ID
	})
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

// FormatBlock renders the context block injected via additionalContext.
// Memories from other projects carry a "from <project>" marker.
func FormatBlock(pk string, mems []Scored, rules []graph.Candidate, maxContentChars int, now int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<infinite-memory project=%q>\n", pk)
	if len(mems) > 0 {
		b.WriteString("Long-term memories from previous sessions (background knowledge; verify before relying on it):\n")
		for _, m := range mems {
			writeLine(&b, m.Candidate, pk, maxContentChars, now)
		}
	}
	if len(rules) > 0 {
		b.WriteString("Standing rules and conventions (follow these unless the user says otherwise):\n")
		for _, r := range rules {
			writeLine(&b, r, pk, maxContentChars, now)
		}
	}
	b.WriteString("</infinite-memory>")
	return b.String()
}

func writeLine(b *strings.Builder, m graph.Candidate, pk string, maxContentChars int, now int64) {
	content := strings.TrimSpace(m.Content)
	if maxContentChars > 0 && len(content) > maxContentChars {
		content = content[:maxContentChars] + "…"
	}
	meta := relAge(now, m.LastSeen)
	if m.ProjectKey != "" && m.ProjectKey != pk {
		meta += ", from " + filepath.Base(m.ProjectKey)
	}
	fmt.Fprintf(b, "- [%s] %s — %s (%s)\n", m.Kind, m.Title, content, meta)
}

// summaryTitleChars caps the title column of the CLI summary.
const summaryTitleChars = 48

// FormatSummary renders the compact body shown to the user in the Claude Code
// CLI: one line per scored memory, standing rules only counted. maxLines caps
// the memory lines (negative means no cap) and everything left over collapses
// into a trailing "… N more · M standing rules" line. Returns "" when there
// is nothing at all.
func FormatSummary(pk string, mems []Scored, rules []graph.Candidate, maxLines int, now int64) string {
	if len(mems) == 0 && len(rules) == 0 {
		return ""
	}
	shown := mems
	if maxLines >= 0 && len(shown) > maxLines {
		shown = shown[:maxLines]
	}

	var lines []string
	for _, m := range shown {
		from := ""
		if m.ProjectKey != "" && m.ProjectKey != pk {
			from = "  ↖" + filepath.Base(m.ProjectKey)
		}
		line := fmt.Sprintf("  %-12s %-*s %8s  %5.1f%s",
			"["+m.Kind+"]", summaryTitleChars, truncRunes(m.Title, summaryTitleChars),
			relAge(now, m.LastSeen), m.Score, from)
		lines = append(lines, strings.TrimRight(line, " "))
	}

	var tail []string
	if rest := len(mems) - len(shown); rest > 0 {
		tail = append(tail, fmt.Sprintf("… %d more", rest))
	}
	if len(rules) > 0 {
		tail = append(tail, fmt.Sprintf("%d standing rules", len(rules)))
	}
	if len(tail) > 0 {
		lines = append(lines, "  "+strings.Join(tail, " · "))
	}
	return strings.Join(lines, "\n")
}

// truncRunes shortens s to max runes (never mid-rune), ellipsis included.
func truncRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max || max <= 0 {
		return s
	}
	if max == 1 {
		return "…"
	}
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}

func relAge(now, ts int64) string {
	d := time.Duration(now-ts) * time.Second
	switch {
	case d < 90*time.Second:
		return "just now"
	case d < 90*time.Minute:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 36*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// SavedLine is one memory the extractor wrote. Deliberately minimal: the save
// side has no score and no age, but the columns line up with FormatSummary so
// the two blocks stack cleanly in the CLI.
type SavedLine struct {
	Title string
	Kind  string
	New   bool
	Seen  int64 // seen_count after the write; shown only when !New
}

// FormatSaved renders the save-side counterpart of FormatSummary: one line per
// written memory, with new/seen-again where retrieve shows age and score.
// maxLines caps the lines (negative means no cap) and the remainder collapses
// into a trailing "… N more". Returns "" when there is nothing.
func FormatSaved(lines []SavedLine, maxLines int) string {
	if len(lines) == 0 {
		return ""
	}
	shown := lines
	if maxLines >= 0 && len(shown) > maxLines {
		shown = shown[:maxLines]
	}

	var out []string
	for _, l := range shown {
		state := "new"
		if !l.New {
			state = fmt.Sprintf("seen %dx", l.Seen)
		}
		line := fmt.Sprintf("  %-12s %-*s  %s",
			"["+l.Kind+"]", summaryTitleChars, truncRunes(l.Title, summaryTitleChars), state)
		out = append(out, strings.TrimRight(line, " "))
	}
	if rest := len(lines) - len(shown); rest > 0 {
		out = append(out, fmt.Sprintf("  … %d more", rest))
	}
	return strings.Join(out, "\n")
}
