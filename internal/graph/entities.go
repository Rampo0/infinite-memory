package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

type EntityInfo struct {
	Name       string `json:"name"`
	Etype      string `json:"etype"`
	ProjectKey string `json:"project_key"`
	Mentions   int64  `json:"mentions"`
}

type EntityRelation struct {
	Name       string `json:"name"`
	Etype      string `json:"etype"`
	ProjectKey string `json:"project_key"`
	Verb       string `json:"verb"`
	Weight     int64  `json:"weight"`
}

type EntityMemory struct {
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	Content    string `json:"content"`
	ProjectKey string `json:"project_key"`
	LastSeen   int64  `json:"last_seen"`
}

type EntityDetail struct {
	Query     string           `json:"query"`
	Nodes     []EntityInfo     `json:"nodes"`
	Relations []EntityRelation `json:"relations"`
	Memories  []EntityMemory   `json:"memories"`
}

// EntityList returns entities ordered by how many memories mention them.
// pk == "" lists across all projects (the default — topics span repos).
func (s *Store) EntityList(ctx context.Context, pk string, limit int) ([]EntityInfo, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)

	where := ""
	params := map[string]any{}
	if pk != "" {
		where = "WHERE e.project_key = $pk"
		params["pk"] = pk
	}
	q := fmt.Sprintf(`
MATCH (e:Entity) %s
OPTIONAL MATCH (e)<-[:MENTIONS]-(m:Memory)
RETURN e.name AS name, e.etype AS etype, e.project_key AS pk, count(m) AS mentions
ORDER BY mentions DESC, name ASC
LIMIT %d`, where, limit)

	res, err := sess.Run(ctx, q, params)
	if err != nil {
		return nil, err
	}
	var out []EntityInfo
	for res.Next(ctx) {
		rec := res.Record()
		out = append(out, EntityInfo{
			Name:       recStr(rec, "name"),
			Etype:      recStr(rec, "etype"),
			ProjectKey: recStr(rec, "pk"),
			Mentions:   recInt(rec, "mentions"),
		})
	}
	return out, res.Err()
}

// EntityDetail returns everything known about an entity name: its nodes
// (one per project it appears in), RELATED neighbors, and the memories that
// mention it. Exact name match first, substring fallback.
func (s *Store) EntityDetail(ctx context.Context, name string) (EntityDetail, error) {
	nlc := strings.ToLower(strings.TrimSpace(name))
	detail := EntityDetail{Query: nlc}
	if nlc == "" {
		return detail, fmt.Errorf("empty entity name")
	}
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)

	match := "e.name_lc = $nlc"
	nodes, err := s.entityNodes(ctx, sess, match, nlc)
	if err != nil {
		return detail, err
	}
	if len(nodes) == 0 {
		match = "e.name_lc CONTAINS $nlc"
		if nodes, err = s.entityNodes(ctx, sess, match, nlc); err != nil {
			return detail, err
		}
	}
	detail.Nodes = nodes
	if len(nodes) == 0 {
		return detail, nil
	}

	qRel := fmt.Sprintf(`
MATCH (e:Entity)-[r:RELATED]-(nb:Entity)
WHERE %s
RETURN nb.name AS name, nb.etype AS etype, nb.project_key AS pk,
       coalesce(r.verb, "") AS verb, coalesce(r.weight, 1) AS weight
ORDER BY weight DESC, name ASC
LIMIT 50`, match)
	res, err := sess.Run(ctx, qRel, map[string]any{"nlc": nlc})
	if err != nil {
		return detail, err
	}
	for res.Next(ctx) {
		rec := res.Record()
		detail.Relations = append(detail.Relations, EntityRelation{
			Name:       recStr(rec, "name"),
			Etype:      recStr(rec, "etype"),
			ProjectKey: recStr(rec, "pk"),
			Verb:       recStr(rec, "verb"),
			Weight:     recInt(rec, "weight"),
		})
	}
	if err := res.Err(); err != nil {
		return detail, err
	}

	qMem := fmt.Sprintf(`
MATCH (e:Entity)<-[:MENTIONS]-(m:Memory)
WHERE %s AND NOT coalesce(m.superseded, false)
RETURN DISTINCT m.title AS title, m.kind AS kind, m.content AS content,
       m.project_key AS pk, m.last_seen_at AS lastSeen
ORDER BY lastSeen DESC
LIMIT 50`, match)
	res, err = sess.Run(ctx, qMem, map[string]any{"nlc": nlc})
	if err != nil {
		return detail, err
	}
	for res.Next(ctx) {
		rec := res.Record()
		detail.Memories = append(detail.Memories, EntityMemory{
			Title:      recStr(rec, "title"),
			Kind:       recStr(rec, "kind"),
			Content:    recStr(rec, "content"),
			ProjectKey: recStr(rec, "pk"),
			LastSeen:   recInt(rec, "lastSeen"),
		})
	}
	return detail, res.Err()
}

func (s *Store) entityNodes(ctx context.Context, sess neo4j.SessionWithContext, match, nlc string) ([]EntityInfo, error) {
	q := fmt.Sprintf(`
MATCH (e:Entity)
WHERE %s
OPTIONAL MATCH (e)<-[:MENTIONS]-(m:Memory)
RETURN e.name AS name, e.etype AS etype, e.project_key AS pk, count(m) AS mentions
ORDER BY mentions DESC
LIMIT 20`, match)
	res, err := sess.Run(ctx, q, map[string]any{"nlc": nlc})
	if err != nil {
		return nil, err
	}
	var out []EntityInfo
	for res.Next(ctx) {
		rec := res.Record()
		out = append(out, EntityInfo{
			Name:       recStr(rec, "name"),
			Etype:      recStr(rec, "etype"),
			ProjectKey: recStr(rec, "pk"),
			Mentions:   recInt(rec, "mentions"),
		})
	}
	return out, res.Err()
}
