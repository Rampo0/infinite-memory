package graph

import (
	"context"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Candidate is one memory returned by a candidate query, with that query's
// hit count. Final scoring/merging happens in internal/retrieve.
type Candidate struct {
	ID        string
	Title     string
	Content   string
	Kind      string
	LastSeen  int64
	SeenCount int64
	Hits      int64
}

const qKeywordHits = `
MATCH (m:Memory {project_key: $pk})
WHERE NOT coalesce(m.superseded, false)
  AND any(t IN $tokens WHERE t IN coalesce(m.keywords, []))
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       size([t IN $tokens WHERE t IN coalesce(m.keywords, [])]) AS hits`

const qEntityHits = `
MATCH (e:Entity {project_key: $pk})<-[:MENTIONS]-(m:Memory)
WHERE (e.name_lc IN $tokens OR any(t IN $tokens WHERE t IN coalesce(e.tokens, [])))
  AND NOT coalesce(m.superseded, false)
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       count(DISTINCT e) AS hits`

const qRelatedHits = `
MATCH (e:Entity {project_key: $pk})-[:RELATED]-(nb:Entity)<-[:MENTIONS]-(m:Memory)
WHERE (e.name_lc IN $tokens OR any(t IN $tokens WHERE t IN coalesce(e.tokens, [])))
  AND NOT (nb.name_lc IN $tokens)
  AND NOT coalesce(m.superseded, false)
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       count(DISTINCT nb) AS hits`

// Candidates runs the three retrieval reads (direct keyword, entity mention,
// 1-hop RELATED expansion) on one Bolt session.
func (s *Store) Candidates(ctx context.Context, pk string, tokens []string) (q1, q2, q3 []Candidate, err error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)

	params := map[string]any{"pk": pk, "tokens": toAny(tokens)}
	run := func(q string) ([]Candidate, error) {
		res, err := sess.Run(ctx, q, params)
		if err != nil {
			return nil, err
		}
		var out []Candidate
		for res.Next(ctx) {
			rec := res.Record()
			out = append(out, Candidate{
				ID:        recStr(rec, "id"),
				Title:     recStr(rec, "title"),
				Content:   recStr(rec, "content"),
				Kind:      recStr(rec, "kind"),
				LastSeen:  recInt(rec, "lastSeen"),
				SeenCount: recInt(rec, "seenCount"),
				Hits:      recInt(rec, "hits"),
			})
		}
		return out, res.Err()
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
