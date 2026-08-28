package graph

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Candidate is one memory returned by a candidate query, with that query's
// hit count. Final scoring/merging happens in internal/retrieve.
type Candidate struct {
	ID         string
	Title      string
	Content    string
	Kind       string
	ProjectKey string
	LastSeen   int64
	SeenCount  int64
	Hits       int64
}

// Retrieval is GLOBAL: memories match by topic (keywords/entities) across
// all projects — a "jago whitelist" memory written in one repo surfaces in
// another. The current project only gets a score boost, applied in
// internal/retrieve, never a hard filter.

const qKeywordHits = `
MATCH (m:Memory)
WHERE NOT coalesce(m.superseded, false)
  AND any(t IN $tokens WHERE t IN coalesce(m.keywords, []))
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       m.project_key AS pk, m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       size([t IN $tokens WHERE t IN coalesce(m.keywords, [])]) AS hits`

const qEntityHits = `
MATCH (e:Entity)<-[:MENTIONS]-(m:Memory)
WHERE (e.name_lc IN $tokens OR any(t IN $tokens WHERE t IN coalesce(e.tokens, [])))
  AND NOT coalesce(m.superseded, false)
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       m.project_key AS pk, m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       count(DISTINCT e) AS hits`

const qRelatedHits = `
MATCH (e:Entity)-[:RELATED]-(nb:Entity)<-[:MENTIONS]-(m:Memory)
WHERE (e.name_lc IN $tokens OR any(t IN $tokens WHERE t IN coalesce(e.tokens, [])))
  AND NOT (nb.name_lc IN $tokens)
  AND NOT coalesce(m.superseded, false)
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       m.project_key AS pk, m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       count(DISTINCT nb) AS hits`

func scanCandidates(ctx context.Context, res neo4j.ResultWithContext) ([]Candidate, error) {
	var out []Candidate
	for res.Next(ctx) {
		rec := res.Record()
		out = append(out, Candidate{
			ID:         recStr(rec, "id"),
			Title:      recStr(rec, "title"),
			Content:    recStr(rec, "content"),
			Kind:       recStr(rec, "kind"),
			ProjectKey: recStr(rec, "pk"),
			LastSeen:   recInt(rec, "lastSeen"),
			SeenCount:  recInt(rec, "seenCount"),
			Hits:       recInt(rec, "hits"),
		})
	}
	return out, res.Err()
}

// Candidates runs the three retrieval reads (direct keyword, entity mention,
// 1-hop RELATED expansion) on one Bolt session, across ALL projects.
func (s *Store) Candidates(ctx context.Context, tokens []string) (q1, q2, q3 []Candidate, err error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)

	params := map[string]any{"tokens": toAny(tokens)}
	run := func(q string) ([]Candidate, error) {
		res, err := sess.Run(ctx, q, params)
		if err != nil {
			return nil, err
		}
		return scanCandidates(ctx, res)
	}

	if q1, err = run(qKeywordHits); err != nil {
		return nil, nil, nil, err
	}
	if q2, err = run(qEntityHits); err != nil {
		return nil, nil, nil, err
	}
	if q3, err = run(qRelatedHits); err != nil {
		return nil, nil, nil, err
	}
	return q1, q2, q3, nil
}

// Rules returns non-superseded kind="rule" memories (standing conventions:
// LOC limits, code style, MR templates, per-repo patterns). limit <= 0 fetches
// all of them, which is the normal call: the query has no ORDER BY, so a
// Cypher-side LIMIT would pick an arbitrary subset. The current-project-first
// ordering and the cap happen in internal/retrieve — the rule population stays
// small, so fetch-then-sort in Go is fine.
func (s *Store) Rules(ctx context.Context, limit int) ([]Candidate, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	q := `MATCH (m:Memory {kind: "rule"})
WHERE NOT coalesce(m.superseded, false)
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       m.project_key AS pk, m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       0 AS hits`

	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	res, err := sess.Run(ctx, q, nil)
	if err != nil {
		return nil, err
	}
	return scanCandidates(ctx, res)
}
