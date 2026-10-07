package graph

import (
	"context"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

type SessionSource struct {
	ID             string
	ProjectKey     string
	TranscriptPath string
	CWD            string
	Agent          bool
	Cursor         int64
	UpdatedAt      int64
}

const qTouchSession = `
MERGE (s:Session {id: $sid})
  ON CREATE SET s.project_key = $pk, s.started_at = $now, s.last_extracted_line = 0
SET s.transcript_path = $path, s.cwd = $cwd, s.agent = coalesce(s.agent, false) OR $agent, s.touched_at = $now`

func (s *Store) TouchSession(ctx context.Context, src SessionSource, now int64) error {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, qTouchSession, map[string]any{
		"sid": src.ID, "pk": src.ProjectKey, "path": src.TranscriptPath, "cwd": src.CWD,
		"agent": src.Agent, "now": now,
	})
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}

const qSweepCandidates = `
MATCH (s:Session)
WHERE s.transcript_path IS NOT NULL AND coalesce(s.touched_at, 0) >= $since
RETURN s.id AS id, coalesce(s.project_key, '') AS pk, s.transcript_path AS path, coalesce(s.cwd, '') AS cwd,
       coalesce(s.agent, false) AS agent, coalesce(s.last_extracted_line, 0) AS cursor,
       coalesce(s.updated_at, 0) AS updated
ORDER BY s.touched_at DESC
LIMIT 500`

func (s *Store) SweepCandidates(ctx context.Context, since int64) ([]SessionSource, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, qSweepCandidates, map[string]any{"since": since})
	if err != nil {
		return nil, err
	}
	var out []SessionSource
	for res.Next(ctx) {
		r := res.Record()
		agent, _ := r.Get("agent")
		ok, _ := agent.(bool)
		out = append(out, SessionSource{
			ID: recStr(r, "id"), ProjectKey: recStr(r, "pk"), TranscriptPath: recStr(r, "path"), CWD: recStr(r, "cwd"),
			Agent: ok, Cursor: recInt(r, "cursor"), UpdatedAt: recInt(r, "updated"),
		})
	}
	return out, res.Err()
}
