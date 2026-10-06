package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/Rampo0/infinite-memory/internal/textutil"
)

// AliasTarget is a memory awaiting index-time aliases.
type AliasTarget struct {
	ID      string
	Title   string
	Content string
	Kind    string
}

// MemoriesWithoutAliases returns live memories saved before the extractor
// produced aliases (the property is absent; an empty list means "done, none"),
// most recently seen first.
func (s *Store) MemoriesWithoutAliases(ctx context.Context, limit int) ([]AliasTarget, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	q := `MATCH (m:Memory)
WHERE m.aliases IS NULL AND NOT coalesce(m.superseded, false) AND NOT coalesce(m.archived, false)
  AND NOT m.project_key STARTS WITH "/itest/"
RETURN m.id AS id, m.title AS title, m.content AS content, m.kind AS kind
ORDER BY m.last_seen_at DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	res, err := sess.Run(ctx, q, nil)
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

// SetAliases stores a memory's aliases and adds their tokens to its keywords,
// so the backfilled words are searchable at once.
func (s *Store) SetAliases(ctx context.Context, id string, aliases []string) error {
	return s.write(ctx, `MATCH (m:Memory {id: $id})
SET m.aliases = $aliases,
    m.keywords = coalesce(m.keywords, []) + [k IN $toks WHERE NOT k IN coalesce(m.keywords, [])]`,
		map[string]any{"id": id, "aliases": toAny(aliases),
			"toks": toAny(textutil.Tokenize(strings.Join(aliases, " "), 24))})
}

// write runs one autocommit write statement.
func (s *Store) write(ctx context.Context, q string, params map[string]any) error {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, q, params)
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}
