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
	Store           *graph.Store
	K               int
	MaxContentChars int
	// SameProjectBoost is added to the score of memories from the current
	// project so local context wins ties without hiding other projects.
	SameProjectBoost float64
	// RulesK caps the always-on standing-rules section (0 disables it).
	RulesK int
}

// Query returns the top-K scored memories for a prompt. Matching is global;
// pk only drives the same-project boost.
func (r *Retriever) Query(ctx context.Context, pk, prompt string, now int64) ([]Scored, error) {
	tokens := textutil.Tokenize(prompt, 24)
	if len(tokens) == 0 {
		return nil, nil
	}
	q1, q2, q3, err := r.Store.Candidates(ctx, tokens)
	if err != nil {
		return nil, err
	}
	scored := MergeAndScore(q1, q2, q3, now, pk, r.SameProjectBoost)
	if len(scored) > r.K {
		scored = scored[:r.K]
	}
	return scored, nil
}

// Retrieve returns the formatted context block ("" when nothing matched and
// no rules exist).
func (r *Retriever) Retrieve(ctx context.Context, pk, prompt string, now int64) (string, int, error) {
	scored, err := r.Query(ctx, pk, prompt, now)
	if err != nil {
		return "", 0, err
	}

	var rules []graph.Candidate
	if r.RulesK > 0 {
		all, err := r.Store.Rules(ctx, 100)
		if err != nil {
			return "", 0, err
		}
		seen := make(map[string]bool, len(scored))
		for _, s := range scored {
			seen[s.ID] = true
		}
		rules = SortRules(all, pk, r.RulesK, seen)
	}

	if len(scored) == 0 && len(rules) == 0 {
		return "", 0, nil
	}
	return FormatBlock(pk, scored, rules, r.MaxContentChars, now), len(scored) + len(rules), nil
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
// they were re-observed and how recently, caps at k and drops ids already
// shown in the scored section.
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
	if len(out) > k {
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
