package graph

import (
	"context"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// ProjectKeys lists the distinct project keys memories were saved under.
func (s *Store) ProjectKeys(ctx context.Context) ([]string, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, "MATCH (m:Memory) WHERE m.project_key IS NOT NULL RETURN DISTINCT m.project_key AS pk ORDER BY pk", nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for res.Next(ctx) {
		out = append(out, recStr(res.Record(), "pk"))
	}
	return out, res.Err()
}

// SetRepoKey gives every memory saved under projectKey the repository it
// belongs to. Additive: project_key, the content hash and every edge stay
// as they are, so the migration can be re-run or undone (REMOVE m.repo_key).
func (s *Store) SetRepoKey(ctx context.Context, projectKey, repoKey string) (int, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, "MATCH (m:Memory {project_key: $pk}) SET m.repo_key = $rk RETURN count(m) AS n",
		map[string]any{"pk": projectKey, "rk": repoKey})
	if err != nil {
		return 0, err
	}
	rec, err := res.Single(ctx)
	if err != nil {
		return 0, err
	}
	return int(recInt(rec, "n")), nil
}
