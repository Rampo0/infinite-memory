// Package retrieve turns a user prompt into a ranked context block:
// tokenize -> graph candidate queries -> score in Go -> top-K -> format.
package retrieve

import (
	"context"
	"fmt"
	"math"
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
}

// Query returns the top-K scored memories for a prompt in a project.
func (r *Retriever) Query(ctx context.Context, pk, prompt string, now int64) ([]Scored, error) {
	tokens := textutil.Tokenize(prompt, 24)
	if len(tokens) == 0 {
		return nil, nil
	}
	q1, q2, q3, err := r.Store.Candidates(ctx, pk, tokens)
	if err != nil {
		return nil, err
	}
	scored := MergeAndScore(q1, q2, q3, now)
	if len(scored) > r.K {
		scored = scored[:r.K]
	}
	return scored, nil
}

// Retrieve returns the formatted context block ("" when nothing matched).
func (r *Retriever) Retrieve(ctx context.Context, pk, prompt string, now int64) (string, int, error) {
	scored, err := r.Query(ctx, pk, prompt, now)
	if err != nil || len(scored) == 0 {
		return "", 0, err
	}
	return FormatBlock(pk, scored, r.MaxContentChars, now), len(scored), nil
}

// MergeAndScore merges the three candidate lists by memory id and ranks:
//
//	match = 1.0*q1 + 2.0*q2 + 0.75*min(q3, 4)   (zero-match discarded)
//	score = match + 2.0*exp(-ageDays/14) + 0.3*ln(1+seen_count)
func MergeAndScore(q1, q2, q3 []graph.Candidate, now int64) []Scored {
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

// FormatBlock renders the context block injected via additionalContext.
func FormatBlock(pk string, mems []Scored, maxContentChars int, now int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<infinite-memory project=%q>\n", pk)
	b.WriteString("Long-term memories from previous sessions (background knowledge; verify before relying on it):\n")
	for _, m := range mems {
		content := strings.TrimSpace(m.Content)
		if maxContentChars > 0 && len(content) > maxContentChars {
			content = content[:maxContentChars] + "…"
		}
		fmt.Fprintf(&b, "- [%s] %s — %s (%s)\n", m.Kind, m.Title, content, relAge(now, m.LastSeen))
	}
	b.WriteString("</infinite-memory>")
	return b.String()
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
