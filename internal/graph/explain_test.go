//go:build integration

package graph

import (
	"context"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// explain asks Memgraph to parse and plan a query without executing it.
func (s *Store) explain(ctx context.Context, stmt string) error {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, "EXPLAIN "+stmt, nil)
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}
