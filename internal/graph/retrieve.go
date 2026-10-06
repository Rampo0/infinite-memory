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
	// Feedback counters (B2): times the extractor judged it used / wrong or
	// outdated, when it was last used, and how often it was injected.
	UsedCount     int64 `json:",omitempty"`
	DisputedCount int64 `json:",omitempty"`
	LastUsed      int64 `json:",omitempty"`
	InjectedCount int64 `json:",omitempty"`
	// Matched lists the query terms (keyword query) or entity names (entity
	// query) this memory matched — the retriever weighs each by its idf.
	Matched []string `json:",omitempty"`
	// Partial lists, for the entity query, query terms that matched only a
	// token of a longer entity name ("imem" in "imem.save"): evidence worth
	// the term, not the rare entity.
	Partial []string `json:",omitempty"`
}

// Retrieval is GLOBAL: memories match by topic (keywords/entities) across
// all projects — a "jago whitelist" memory written in one repo surfaces in
// another. The current project only gets a score boost, applied in
// internal/retrieve, never a hard filter.

const qKeywordHits = `
MATCH (m:Memory)
WHERE NOT coalesce(m.superseded, false) AND NOT coalesce(m.archived, false)
  AND any(t IN $tokens WHERE t IN coalesce(m.keywords, []))
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       coalesce(m.repo_key, m.project_key) AS pk, m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       coalesce(m.used_count, 0) AS usedCount, coalesce(m.disputed_count, 0) AS disputedCount,
       coalesce(m.last_used_at, 0) AS lastUsed, coalesce(m.injected_count, 0) AS injectedCount,
       [t IN $tokens WHERE t IN coalesce(m.keywords, [])] AS matched,
       size([t IN $tokens WHERE t IN coalesce(m.keywords, [])]) AS hits`

// Entity hits split exact name matches (matched) from terms that only hit a
// token of a longer entity name (partial); the retriever weighs them apart.
const qEntityHits = `
MATCH (e:Entity)<-[:MENTIONS]-(m:Memory)
WHERE (e.name_lc IN $tokens OR any(t IN $tokens WHERE t IN coalesce(e.tokens, [])))
  AND NOT coalesce(m.superseded, false) AND NOT coalesce(m.archived, false)
WITH m, e,
     CASE WHEN e.name_lc IN $tokens THEN e.name_lc ELSE null END AS exactName,
     [t IN $tokens WHERE t IN coalesce(e.tokens, []) AND t <> e.name_lc] AS partialTerms
WITH m, collect(DISTINCT exactName) AS exact, collect(partialTerms) AS partials, count(DISTINCT e) AS hits
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       coalesce(m.repo_key, m.project_key) AS pk, m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       coalesce(m.used_count, 0) AS usedCount, coalesce(m.disputed_count, 0) AS disputedCount,
       coalesce(m.last_used_at, 0) AS lastUsed, coalesce(m.injected_count, 0) AS injectedCount,
       exact AS matched, reduce(acc = [], l IN partials | acc + l) AS partial, hits`

// Only edges seen at least $minRel times: a weight-1 edge is one chance
// co-occurrence (72% of all edges), and following them pulled in most of the
// graph for a single prompt.
const qRelatedHits = `
MATCH (e:Entity)-[r:RELATED]-(nb:Entity)<-[:MENTIONS]-(m:Memory)
WHERE (e.name_lc IN $tokens OR any(t IN $tokens WHERE t IN coalesce(e.tokens, [])))
  AND coalesce(r.weight, 1) >= $minRel
  AND NOT (nb.name_lc IN $tokens)
  AND NOT coalesce(m.superseded, false) AND NOT coalesce(m.archived, false)
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       coalesce(m.repo_key, m.project_key) AS pk, m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       coalesce(m.used_count, 0) AS usedCount, coalesce(m.disputed_count, 0) AS disputedCount,
       coalesce(m.last_used_at, 0) AS lastUsed, coalesce(m.injected_count, 0) AS injectedCount,
       count(DISTINCT nb) AS hits`

func dedupe(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ss))
	out := ss[:0:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func scanCandidates(ctx context.Context, res neo4j.ResultWithContext) ([]Candidate, error) {
	var out []Candidate
	for res.Next(ctx) {
		rec := res.Record()
		out = append(out, Candidate{
			ID:            recStr(rec, "id"),
			Title:         recStr(rec, "title"),
			Content:       recStr(rec, "content"),
			Kind:          recStr(rec, "kind"),
			ProjectKey:    recStr(rec, "pk"),
			LastSeen:      recInt(rec, "lastSeen"),
			SeenCount:     recInt(rec, "seenCount"),
			Hits:          recInt(rec, "hits"),
			Matched:       recStrs(rec, "matched"),
			UsedCount:     recInt(rec, "usedCount"),
			DisputedCount: recInt(rec, "disputedCount"),
			LastUsed:      recInt(rec, "lastUsed"),
			InjectedCount: recInt(rec, "injectedCount"),
			Partial:       dedupe(recStrs(rec, "partial")),
		})
	}
	return out, res.Err()
}

// Candidates runs the three retrieval reads (direct keyword, entity mention,
// 1-hop RELATED expansion over edges of weight >= minRel) on one Bolt
// session, across ALL projects.
func (s *Store) Candidates(ctx context.Context, tokens []string, minRel int) (q1, q2, q3 []Candidate, err error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)

	if minRel < 1 {
		minRel = 1
	}
	params := map[string]any{"tokens": toAny(tokens), "minRel": int64(minRel)}
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
	return s.ByKind(ctx, "rule", limit)
}

// ByKind returns the non-superseded memories of one kind (rule, preference…),
// unordered; limit <= 0 fetches all.
func (s *Store) ByKind(ctx context.Context, kind string, limit int) ([]Candidate, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	q := `MATCH (m:Memory {kind: $kind})
WHERE NOT coalesce(m.superseded, false) AND NOT coalesce(m.archived, false)
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       coalesce(m.repo_key, m.project_key) AS pk, m.last_seen_at AS lastSeen, m.seen_count AS seenCount,
       coalesce(m.used_count, 0) AS usedCount, coalesce(m.disputed_count, 0) AS disputedCount,
       coalesce(m.last_used_at, 0) AS lastUsed, coalesce(m.injected_count, 0) AS injectedCount,
       0 AS hits`

	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	res, err := sess.Run(ctx, q, map[string]any{"kind": kind})
	if err != nil {
		return nil, err
	}
	return scanCandidates(ctx, res)
}

// CountLive is the number of non-superseded memories: the N of the
// retriever's idf weights.
func (s *Store) CountLive(ctx context.Context) (int, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, "MATCH (m:Memory) WHERE NOT coalesce(m.superseded, false) AND NOT coalesce(m.archived, false) RETURN count(m) AS n", nil)
	if err != nil {
		return 0, err
	}
	if res.Next(ctx) {
		return int(recInt(res.Record(), "n")), res.Err()
	}
	return 0, res.Err()
}
