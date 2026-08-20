package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/Rampo0/infinite-memory/internal/textutil"
)

type EntityIn struct {
	Name string
	Type string
}

type MemoryIn struct {
	Title    string
	Content  string
	Kind     string
	Entities []EntityIn
	// Relations are [a, verb, b] triples between entities of this memory.
	Relations [][3]string
}

// HashContent is the dedup key: same content in the same project collapses
// into one Memory whose seen_count grows.
func HashContent(projectKey, content string) string {
	sum := sha256.Sum256([]byte(projectKey + "\x00" + strings.ToLower(strings.TrimSpace(content))))
	return hex.EncodeToString(sum[:])
}

const qUpsertProject = `
MERGE (p:Project {key: $pk})
  ON CREATE SET p.name = $pname, p.created_at = $now`

const qUpsertSession = `
MERGE (s:Session {id: $sid})
  ON CREATE SET s.project_key = $pk, s.started_at = $now, s.last_extracted_line = 0
SET s.updated_at = $now
WITH s
MATCH (p:Project {key: $pk})
MERGE (s)-[:IN_PROJECT]->(p)`

const qUpsertMemory = `
MERGE (m:Memory {hash: $hash})
  ON CREATE SET m.id = $id, m.title = $title, m.title_lc = $title_lc,
                m.content = $content, m.kind = $kind, m.keywords = $keywords,
                m.project_key = $pk, m.created_at = $now, m.seen_count = 1,
                m.superseded = false, m.source_session = $sid
  ON MATCH  SET m.seen_count = m.seen_count + 1
SET m.last_seen_at = $now
WITH m
MATCH (p:Project {key: $pk}) MERGE (m)-[:IN_PROJECT]->(p)
WITH m
MATCH (s:Session {id: $sid}) MERGE (m)-[:IN_SESSION]->(s)
RETURN m.seen_count AS sc`

const qSupersede = `
MATCH (old:Memory {project_key: $pk, title_lc: $title_lc, kind: $kind})
WHERE old.hash <> $hash AND NOT coalesce(old.superseded, false)
MATCH (new:Memory {hash: $hash})
MERGE (new)-[:SUPERSEDES]->(old)
SET old.superseded = true`

const qUpsertEntity = `
MERGE (e:Entity {key: $ekey})
  ON CREATE SET e.name = $name, e.name_lc = $name_lc, e.tokens = $etokens,
                e.etype = $etype, e.project_key = $pk, e.created_at = $now
WITH e
MATCH (m:Memory {hash: $hash})
MERGE (m)-[:MENTIONS]->(e)`

const qRelated = `
MATCH (a:Entity {key: $k1}), (b:Entity {key: $k2})
MERGE (a)-[r:RELATED]->(b)
  ON CREATE SET r.weight = 1
  ON MATCH  SET r.weight = r.weight + 1
SET r.last_seen_at = $now`

const qRelatedVerb = `
MATCH (a:Entity {key: $k1}), (b:Entity {key: $k2})
MERGE (a)-[r:RELATED]->(b)
  ON CREATE SET r.weight = 1, r.verb = $verb
  ON MATCH  SET r.weight = r.weight + 1
SET r.last_seen_at = $now`

func txRun(ctx context.Context, tx neo4j.ManagedTransaction, cypher string, params map[string]any) error {
	res, err := tx.Run(ctx, cypher, params)
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}

// SaveBatch writes one extraction batch in a single transaction: project and
// session upserts, then per memory the MERGE/supersede plus entities,
// mentions and RELATED co-occurrence edges.
func (s *Store) SaveBatch(ctx context.Context, pk, sid string, now int64, mems []MemoryIn) (int, error) {
	if len(mems) == 0 {
		return 0, nil
	}
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)

	saved := 0
	_, err := sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		base := map[string]any{"pk": pk, "pname": projectName(pk), "sid": sid, "now": now}
		if err := txRun(ctx, tx, qUpsertProject, base); err != nil {
			return nil, err
		}
		if err := txRun(ctx, tx, qUpsertSession, base); err != nil {
			return nil, err
		}

		for _, m := range mems {
			hash := HashContent(pk, m.Content)
			titleLC := strings.ToLower(strings.TrimSpace(m.Title))
			res, err := tx.Run(ctx, qUpsertMemory, map[string]any{
				"hash": hash, "id": hash[:16],
				"title": m.Title, "title_lc": titleLC,
				"content": m.Content, "kind": m.Kind,
				"keywords": toAny(textutil.Tokenize(m.Title+" "+m.Content, 32)),
				"pk":       pk, "now": now, "sid": sid,
			})
			if err != nil {
				return nil, err
			}
			rec, err := res.Single(ctx)
			if err != nil {
				return nil, err
			}
			created := recInt(rec, "sc") == 1
			if created {
				err := txRun(ctx, tx, qSupersede, map[string]any{
					"pk": pk, "title_lc": titleLC, "kind": m.Kind, "hash": hash,
				})
				if err != nil {
					return nil, err
				}
			}

			// Entities + MENTIONS. keyByName maps name_lc -> entity key for
			// relation triples below.
			keyByName := make(map[string]string, len(m.Entities))
			for _, e := range m.Entities {
				name := strings.TrimSpace(e.Name)
				if name == "" {
					continue
				}
				nameLC := strings.ToLower(name)
				ekey := pk + "\x00" + nameLC
				if _, dup := keyByName[nameLC]; dup {
					continue
				}
				keyByName[nameLC] = ekey
				etype := strings.TrimSpace(e.Type)
				if etype == "" {
					etype = "other"
				}
				err := txRun(ctx, tx, qUpsertEntity, map[string]any{
					"ekey": ekey, "name": name, "name_lc": nameLC,
					"etokens": toAny(textutil.Tokenize(name, 8)),
					"etype":   etype, "pk": pk, "now": now, "hash": hash,
				})
				if err != nil {
					return nil, err
				}
			}

			// RELATED edges: co-occurrence pairs of this memory's entities,
			// with explicit relation triples supplying a verb when given.
			// Canonical direction k1 < k2 keeps each pair a single edge.
			verbByPair := map[[2]string]string{}
			keys := make([]string, 0, len(keyByName))
			for _, k := range keyByName {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for i := 0; i < len(keys); i++ {
				for j := i + 1; j < len(keys); j++ {
					verbByPair[[2]string{keys[i], keys[j]}] = ""
				}
			}
			for _, rel := range m.Relations {
				ka, oka := keyByName[strings.ToLower(strings.TrimSpace(rel[0]))]
				kb, okb := keyByName[strings.ToLower(strings.TrimSpace(rel[2]))]
				if !oka || !okb || ka == kb {
					continue
				}
				if ka > kb {
					ka, kb = kb, ka
				}
				verbByPair[[2]string{ka, kb}] = strings.TrimSpace(rel[1])
			}
			for pair, verb := range verbByPair {
				params := map[string]any{"k1": pair[0], "k2": pair[1], "now": now}
				q := qRelated
				if verb != "" {
					q = qRelatedVerb
					params["verb"] = verb
				}
				if err := txRun(ctx, tx, q, params); err != nil {
					return nil, err
				}
			}
			saved++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return saved, nil
}

func projectName(pk string) string {
	if i := strings.LastIndexByte(pk, '/'); i >= 0 && i+1 < len(pk) {
		return pk[i+1:]
	}
	return pk
}

// GetCursor returns how many transcript JSONL lines of this session were
// already extracted (0 when the session is unknown).
func (s *Store) GetCursor(ctx context.Context, sid string) (int64, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx,
		"MATCH (s:Session {id: $sid}) RETURN coalesce(s.last_extracted_line, 0) AS l",
		map[string]any{"sid": sid})
	if err != nil {
		return 0, err
	}
	if res.Next(ctx) {
		return recInt(res.Record(), "l"), res.Err()
	}
	return 0, res.Err()
}

const qSetCursor = `
MERGE (p:Project {key: $pk})
  ON CREATE SET p.name = $pname, p.created_at = $now
MERGE (s:Session {id: $sid})
  ON CREATE SET s.project_key = $pk, s.started_at = $now
SET s.last_extracted_line = $line, s.updated_at = $now
MERGE (s)-[:IN_PROJECT]->(p)`

func (s *Store) SetCursor(ctx context.Context, sid, pk string, line, now int64) error {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, qSetCursor, map[string]any{
		"sid": sid, "pk": pk, "pname": projectName(pk), "line": line, "now": now,
	})
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}

// EntityNames returns up to limit recent entity names for the project,
// used as canonicalization hints in the extraction prompt.
func (s *Store) EntityNames(ctx context.Context, pk string, limit int) ([]string, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	q := fmt.Sprintf(
		"MATCH (e:Entity {project_key: $pk}) RETURN e.name AS name ORDER BY e.created_at DESC LIMIT %d",
		limit)
	res, err := sess.Run(ctx, q, map[string]any{"pk": pk})
	if err != nil {
		return nil, err
	}
	var names []string
	for res.Next(ctx) {
		if n := recStr(res.Record(), "name"); n != "" {
			names = append(names, n)
		}
	}
	return names, res.Err()
}

// EntityNamesGlobal returns recent entity names across all projects, used as
// cross-repo canonicalization hints (topics span repositories).
func (s *Store) EntityNamesGlobal(ctx context.Context, limit int) ([]string, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	q := fmt.Sprintf(
		"MATCH (e:Entity) RETURN e.name AS name ORDER BY e.created_at DESC LIMIT %d",
		limit)
	res, err := sess.Run(ctx, q, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for res.Next(ctx) {
		if n := recStr(res.Record(), "name"); n != "" {
			names = append(names, n)
		}
	}
	return names, res.Err()
}

type ProjectStats struct {
	Key      string `json:"key"`
	Memories int64  `json:"memories"`
	Entities int64  `json:"entities"`
	Sessions int64  `json:"sessions"`
}

func (s *Store) Stats(ctx context.Context) ([]ProjectStats, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	byKey := map[string]*ProjectStats{}
	get := func(k string) *ProjectStats {
		if byKey[k] == nil {
			byKey[k] = &ProjectStats{Key: k}
		}
		return byKey[k]
	}
	type agg struct {
		q   string
		set func(st *ProjectStats, c int64)
	}
	aggs := []agg{
		{"MATCH (m:Memory) RETURN m.project_key AS pk, count(*) AS c",
			func(st *ProjectStats, c int64) { st.Memories = c }},
		{"MATCH (e:Entity) RETURN e.project_key AS pk, count(*) AS c",
			func(st *ProjectStats, c int64) { st.Entities = c }},
		{"MATCH (s:Session) RETURN s.project_key AS pk, count(*) AS c",
			func(st *ProjectStats, c int64) { st.Sessions = c }},
	}
	for _, a := range aggs {
		res, err := sess.Run(ctx, a.q, nil)
		if err != nil {
			return nil, err
		}
		for res.Next(ctx) {
			rec := res.Record()
			a.set(get(recStr(rec, "pk")), recInt(rec, "c"))
		}
		if err := res.Err(); err != nil {
			return nil, err
		}
	}
	out := make([]ProjectStats, 0, len(byKey))
	for _, st := range byKey {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
