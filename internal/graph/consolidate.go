package graph

import (
	"context"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// LiveMemory is a retrievable memory with what consolidation clusters on.
type LiveMemory struct {
	ID, Title, Content, Kind, ProjectKey string
	Keywords, Entities, Aliases          []string
}

// LiveMemories lists every retrievable memory (not superseded, not archived,
// not an /itest/ fixture) with its keywords and lowercased entity names.
func (s *Store) LiveMemories(ctx context.Context) ([]LiveMemory, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, `MATCH (m:Memory)
WHERE NOT coalesce(m.superseded, false) AND NOT coalesce(m.archived, false)
  AND NOT m.project_key STARTS WITH "/itest/"
OPTIONAL MATCH (m)-[:MENTIONS]->(e:Entity)
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind,
       coalesce(m.repo_key, m.project_key) AS pk, coalesce(m.keywords, []) AS kw,
       coalesce(m.aliases, []) AS al,
       m.last_seen_at AS ls, collect(DISTINCT e.name_lc) AS ents
ORDER BY ls DESC`, nil)
	if err != nil {
		return nil, err
	}
	var out []LiveMemory
	for res.Next(ctx) {
		rec := res.Record()
		out = append(out, LiveMemory{ID: recStr(rec, "id"), Title: recStr(rec, "title"),
			Content: recStr(rec, "content"), Kind: recStr(rec, "kind"), ProjectKey: recStr(rec, "pk"),
			Keywords: recStrs(rec, "kw"), Entities: recStrs(rec, "ents"), Aliases: recStrs(rec, "al")})
	}
	return out, res.Err()
}

// SupersedeBy retires ids in favour of keeper (skipping keeper itself) and
// returns how many it retired. Non-destructive: the nodes and their edges stay.
func (s *Store) SupersedeBy(ctx context.Context, keeper string, ids []string) (int, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, `MATCH (k:Memory {id: $keeper})
UNWIND $ids AS id
MATCH (old:Memory {id: id})
WHERE old.id <> $keeper AND NOT coalesce(old.superseded, false)
MERGE (k)-[:SUPERSEDES]->(old)
SET old.superseded = true
RETURN count(old) AS n`, map[string]any{"keeper": keeper, "ids": toAny(ids)})
	if err != nil {
		return 0, err
	}
	rec, err := res.Single(ctx)
	if err != nil {
		return 0, err
	}
	return int(recInt(rec, "n")), nil
}

// ArchiveCandidates are live memories injected at least minInjected times,
// never judged used, and not seen since lastSeenBefore: they keep matching
// prompts and never help.
func (s *Store) ArchiveCandidates(ctx context.Context, minInjected int, lastSeenBefore int64) ([]AliasTarget, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, `MATCH (m:Memory)
WHERE NOT coalesce(m.superseded, false) AND NOT coalesce(m.archived, false)
  AND coalesce(m.injected_count, 0) >= $minInjected AND coalesce(m.used_count, 0) = 0
  AND m.last_seen_at < $before
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind
ORDER BY m.injected_count DESC`, map[string]any{"minInjected": int64(minInjected), "before": lastSeenBefore})
	if err != nil {
		return nil, err
	}
	var out []AliasTarget
	for res.Next(ctx) {
		rec := res.Record()
		out = append(out, AliasTarget{ID: recStr(rec, "id"), Title: recStr(rec, "title"),
			Content: recStr(rec, "content"), Kind: recStr(rec, "kind")})
	}
	return out, res.Err()
}

// Archive takes memories out of retrieval without deleting them (undo:
// REMOVE m.archived).
func (s *Store) Archive(ctx context.Context, ids []string) error {
	return s.write(ctx, "UNWIND $ids AS id MATCH (m:Memory {id: id}) SET m.archived = true",
		map[string]any{"ids": toAny(ids)})
}
