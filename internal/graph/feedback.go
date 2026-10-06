package graph

import "context"

// Verdict grades one memory the assistant was shown: used, wrong or outdated.
type Verdict struct {
	ID      string
	Verdict string
}

// ApplyFeedback records the extractor's grades. "used" counts and refreshes
// last_used_at; "wrong" and "outdated" only count as disputes — retiring a
// memory takes a reconcile update naming its replacement. Unknown ids are
// ignored.
func (s *Store) ApplyFeedback(ctx context.Context, now int64, vs []Verdict) error {
	for _, v := range vs {
		q := `MATCH (m:Memory {id: $id})
SET m.used_count = coalesce(m.used_count, 0) + 1, m.last_used_at = $now`
		if v.Verdict != "used" {
			q = `MATCH (m:Memory {id: $id})
SET m.disputed_count = coalesce(m.disputed_count, 0) + 1`
		}
		if err := s.write(ctx, q, map[string]any{"id": v.ID, "now": now}); err != nil {
			return err
		}
	}
	return nil
}

// MarkInjected counts how often each memory was put in front of a model; with
// used_count it tells a useful memory from one that only ever matched.
func (s *Store) MarkInjected(ctx context.Context, ids []string, now int64) error {
	if len(ids) == 0 {
		return nil
	}
	return s.write(ctx, `UNWIND $ids AS id
MATCH (m:Memory {id: id})
SET m.injected_count = coalesce(m.injected_count, 0) + 1, m.last_injected_at = $now`,
		map[string]any{"ids": toAny(ids), "now": now})
}
