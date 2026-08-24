// Package graph owns all Memgraph access: schema, writes (save/cursor) and
// the retrieval candidate queries. Only the daemon, `imem init` and
// `imem restore` construct a Store — hook subcommands never touch Bolt.
// `imem restore` is direct rather than daemon-mediated on purpose: a restore
// is needed precisely when the daemon is down.
package graph

import (
	"context"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

type Store struct {
	driver neo4j.DriverWithContext
}

func New(uri, user, pass string) (*Store, error) {
	auth := neo4j.NoAuth()
	if user != "" {
		auth = neo4j.BasicAuth(user, pass, "")
	}
	driver, err := neo4j.NewDriverWithContext(uri, auth)
	if err != nil {
		return nil, err
	}
	return &Store{driver: driver}, nil
}

func (s *Store) Close(ctx context.Context) error { return s.driver.Close(ctx) }

func (s *Store) Ping(ctx context.Context) error {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, "RETURN 1", nil)
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}

// Memgraph syntax: constraints do not create indexes, so both are declared.
var schemaStatements = []string{
	"CREATE CONSTRAINT ON (p:Project) ASSERT p.key IS UNIQUE",
	"CREATE CONSTRAINT ON (s:Session) ASSERT s.id IS UNIQUE",
	"CREATE CONSTRAINT ON (m:Memory) ASSERT m.hash IS UNIQUE",
	"CREATE CONSTRAINT ON (e:Entity) ASSERT e.key IS UNIQUE",
	"CREATE INDEX ON :Project(key)",
	"CREATE INDEX ON :Session(id)",
	"CREATE INDEX ON :Memory(hash)",
	"CREATE INDEX ON :Memory(project_key)",
	"CREATE INDEX ON :Memory(title_lc)",
	"CREATE INDEX ON :Entity(key)",
	"CREATE INDEX ON :Entity(project_key)",
	"CREATE INDEX ON :Entity(name_lc)",
}

// EnsureSchema applies constraints and indexes one by one, tolerating
// re-runs. DDL must run outside explicit transactions in Memgraph, hence
// autocommit session.Run per statement.
func (s *Store) EnsureSchema(ctx context.Context) error {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	for _, stmt := range schemaStatements {
		res, err := sess.Run(ctx, stmt, nil)
		if err == nil {
			_, err = res.Consume(ctx)
		}
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "exist") {
			return err
		}
	}
	return nil
}

// ShowSchema returns human-readable index and constraint listings.
func (s *Store) ShowSchema(ctx context.Context) ([]string, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	var lines []string
	for _, stmt := range []string{"SHOW INDEX INFO", "SHOW CONSTRAINT INFO"} {
		res, err := sess.Run(ctx, stmt, nil)
		if err != nil {
			return nil, err
		}
		for res.Next(ctx) {
			var parts []string
			for _, v := range res.Record().Values {
				if v != nil {
					if str, ok := v.(string); ok {
						parts = append(parts, str)
					}
				}
			}
			lines = append(lines, strings.Join(parts, " "))
		}
		if err := res.Err(); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func recStr(rec *neo4j.Record, key string) string {
	v, _ := rec.Get(key)
	s, _ := v.(string)
	return s
}

func recInt(rec *neo4j.Record, key string) int64 {
	v, _ := rec.Get(key)
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
