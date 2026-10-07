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
WITH m, [k IN $toks WHERE NOT k IN coalesce(m.keywords, [])] AS fresh
SET m.aliases = $aliases,
    m.keywords = coalesce(m.keywords, []) + fresh,
    m.alias_only = coalesce(m.alias_only, []) + fresh`,
		map[string]any{"id": id, "aliases": toAny(aliases),
			"toks": toAny(textutil.Tokenize(strings.Join(aliases, " "), 24))})
}

const maxAliases = 20

func (s *Store) LearnAliases(ctx context.Context, id string, terms []string) error {
	return s.write(ctx, `MATCH (m:Memory {id: $id})
WHERE size(coalesce(m.aliases, [])) < $cap
WITH m, [a IN $terms WHERE NOT a IN coalesce(m.aliases, [])] AS added,
     [k IN $terms WHERE NOT k IN coalesce(m.keywords, [])] AS fresh
SET m.aliases = coalesce(m.aliases, []) + added,
    m.keywords = coalesce(m.keywords, []) + fresh,
    m.alias_only = coalesce(m.alias_only, []) + fresh,
    m.learned_aliases = coalesce(m.learned_aliases, 0) + size(added)`,
		map[string]any{"id": id, "terms": toAny(terms), "cap": maxAliases})
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

// IndexRow is one memory's text and token lists, for `imem reindex`.
type IndexRow struct {
	ID, Title, Content           string
	Aliases, Keywords, AliasOnly []string
}

// IndexRows lists every memory with what reindexing needs.
func (s *Store) IndexRows(ctx context.Context) ([]IndexRow, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, `MATCH (m:Memory)
RETURN m.id AS id, m.title AS title, m.content AS content, coalesce(m.aliases, []) AS al,
       coalesce(m.keywords, []) AS kw, coalesce(m.alias_only, []) AS ao`, nil)
	if err != nil {
		return nil, err
	}
	var out []IndexRow
	for res.Next(ctx) {
		rec := res.Record()
		out = append(out, IndexRow{ID: recStr(rec, "id"), Title: recStr(rec, "title"), Content: recStr(rec, "content"),
			Aliases: recStrs(rec, "al"), Keywords: recStrs(rec, "kw"), AliasOnly: recStrs(rec, "ao")})
	}
	return out, res.Err()
}

// SetIndex replaces a memory's token lists.
func (s *Store) SetIndex(ctx context.Context, id string, keywords, aliasOnly []string) error {
	return s.write(ctx, "MATCH (m:Memory {id: $id}) SET m.keywords = $kw, m.alias_only = $ao",
		map[string]any{"id": id, "kw": toAny(keywords), "ao": toAny(aliasOnly)})
}

// IndexChange is one memory's recomputed token lists.
type IndexChange struct {
	ID                  string
	Keywords, AliasOnly []string
}

// ReindexPlan recomputes every row's keywords and alias-only tokens with the
// current tokenizer (stopwords and caps change over time; stored lists do
// not) and returns the rows whose lists differ.
func ReindexPlan(rows []IndexRow) []IndexChange {
	var out []IndexChange
	for _, r := range rows {
		m := MemoryIn{Title: r.Title, Content: r.Content, Aliases: r.Aliases}
		kw, ao := MemoryKeywords(m), AliasOnly(m)
		if sameSet(kw, r.Keywords) && sameSet(ao, r.AliasOnly) {
			continue
		}
		out = append(out, IndexChange{ID: r.ID, Keywords: kw, AliasOnly: ao})
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, x := range a {
		set[x] = true
	}
	for _, x := range b {
		if !set[x] {
			return false
		}
	}
	return true
}
