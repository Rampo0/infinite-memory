package graph

import (
	"context"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

type PinTarget struct {
	ID     string
	Title  string
	Kind   string
	Pinned bool
}

const qPinTargets = `MATCH (m:Memory)
WHERE m.kind IN ['rule', 'preference']
  AND NOT coalesce(m.superseded, false) AND NOT coalesce(m.archived, false)
  AND (m.id = $q OR toLower(m.title) CONTAINS toLower($q))
RETURN m.id AS id, m.title AS title, m.kind AS kind, coalesce(m.pinned, false) AS pinned
ORDER BY m.id = $q DESC, m.title
LIMIT 20`

func (s *Store) PinTargets(ctx context.Context, q string) ([]PinTarget, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, qPinTargets, map[string]any{"q": q})
	if err != nil {
		return nil, err
	}
	var out []PinTarget
	for res.Next(ctx) {
		rec := res.Record()
		out = append(out, PinTarget{ID: recStr(rec, "id"), Title: recStr(rec, "title"),
			Kind: recStr(rec, "kind"), Pinned: recBool(rec, "pinned")})
	}
	return out, res.Err()
}

func (s *Store) SetPinned(ctx context.Context, id string, pinned bool) error {
	return s.write(ctx, `MATCH (m:Memory {id: $id})
WHERE m.kind IN ['rule', 'preference']
SET m.pinned = $pinned`, map[string]any{"id": id, "pinned": pinned})
}
