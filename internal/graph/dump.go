package graph

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Dump returns the full logical backup of the graph as an ordered list of
// Cypher statements (Memgraph's DUMP DATABASE). Order matters: nodes, the
// temporary __mg_vertex__ index, edges, the __mg_id__ cleanup, then indexes
// and constraints. Replaying the list in order reconstructs data and schema.
func (s *Store) Dump(ctx context.Context) ([]string, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, "DUMP DATABASE", nil)
	if err != nil {
		return nil, err
	}
	var stmts []string
	for res.Next(ctx) {
		if q := recStr(res.Record(), "QUERY"); q != "" {
			stmts = append(stmts, q)
		}
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return stmts, nil
}

// transactionalWipe is the fallback for DROP GRAPH, which Memgraph only
// accepts in IN_MEMORY_ANALYTICAL mode. This daemon runs the default
// IN_MEMORY_TRANSACTIONAL, so in practice this is the path that gets used.
var transactionalWipe = []string{
	"MATCH (n) DETACH DELETE n",
	"DROP ALL CONSTRAINTS",
	"DROP ALL INDEXES",
}

// Restore wipes the graph and replays a dump. The wipe is required: the dump
// recreates unique constraints that would collide with surviving rows.
// Statements run autocommit one by one because Memgraph rejects DDL inside an
// explicit transaction.
func (s *Store) Restore(ctx context.Context, stmts []string) error {
	if len(stmts) == 0 {
		return fmt.Errorf("refusing to restore an empty dump")
	}
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)

	// DROP GRAPH is one efficient statement but is analytical-mode only, so
	// fall back to clearing data and schema by hand when it is rejected.
	if err := run(ctx, sess, "DROP GRAPH"); err != nil {
		for _, stmt := range transactionalWipe {
			if werr := run(ctx, sess, stmt); werr != nil {
				return fmt.Errorf("wipe (%s): %w (drop graph said: %v)", stmt, werr, err)
			}
		}
	}

	for i, stmt := range stmts {
		if err := run(ctx, sess, stmt); err != nil {
			return fmt.Errorf("statement %d: %w", i+1, err)
		}
	}
	return nil
}

func run(ctx context.Context, sess neo4j.SessionWithContext, stmt string) error {
	res, err := sess.Run(ctx, stmt, nil)
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}
