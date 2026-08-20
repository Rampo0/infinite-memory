//go:build integration

package graph

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Requires a running Memgraph (make up). Run with: make itest

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := New("bolt://127.0.0.1:7687", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		t.Skipf("memgraph not reachable: %v", err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

func seedBatch() []MemoryIn {
	return []MemoryIn{
		{
			Title: "Daemon port decision", Content: "The imem daemon listens on 127.0.0.1:7690.", Kind: "decision",
			Entities:  []EntityIn{{Name: "imem daemon", Type: "component"}, {Name: "Memgraph", Type: "technology"}},
			Relations: [][3]string{{"imem daemon", "uses", "Memgraph"}},
		},
		{
			Title: "Retrieval scoring", Content: "Scoring merges keyword and entity hits with recency decay.", Kind: "fact",
			Entities: []EntityIn{{Name: "Memgraph", Type: "technology"}, {Name: "retrieval", Type: "concept"}},
		},
	}
}

func TestSaveAndCandidates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d", time.Now().UnixNano())
	now := time.Now().Unix()

	saved, err := s.SaveBatch(ctx, pk, "sess-1", now, seedBatch())
	if err != nil {
		t.Fatal(err)
	}
	if saved != 2 {
		t.Fatalf("want 2 saved, got %d", saved)
	}

	// Q1: keyword match on "7690" is dropped (integer) — use "listens"/"daemon".
	q1, q2, q3, err := s.Candidates(ctx, pk, []string{"daemon", "port"})
	if err != nil {
		t.Fatal(err)
	}
	if len(q1) == 0 {
		t.Fatalf("keyword query found nothing: q1=%v", q1)
	}
	if len(q2) == 0 {
		t.Fatalf("entity query found nothing (entity tokens should match 'daemon')")
	}
	_ = q3

	// Q2 exact entity name.
	_, q2b, _, err := s.Candidates(ctx, pk, []string{"memgraph"})
	if err != nil {
		t.Fatal(err)
	}
	if len(q2b) != 2 {
		t.Fatalf("memgraph entity should hit both memories, got %d", len(q2b))
	}

	// Q3: 1-hop — "retrieval" relates to "memgraph" via co-occurrence, and
	// memgraph mentions the port memory, so expansion should reach it.
	_, _, q3b, err := s.Candidates(ctx, pk, []string{"retrieval"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range q3b {
		if c.Title == "Daemon port decision" {
			found = true
		}
	}
	if !found {
		t.Fatalf("1-hop expansion should surface the port memory, got %+v", q3b)
	}
}

func TestDedupBumpsSeenCount(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d", time.Now().UnixNano())
	now := time.Now().Unix()

	batch := seedBatch()[:1]
	if _, err := s.SaveBatch(ctx, pk, "sess-1", now, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBatch(ctx, pk, "sess-2", now+10, batch); err != nil {
		t.Fatal(err)
	}
	q1, _, _, err := s.Candidates(ctx, pk, []string{"listens"})
	if err != nil {
		t.Fatal(err)
	}
	if len(q1) != 1 || q1[0].SeenCount != 2 {
		t.Fatalf("duplicate save should bump seen_count to 2, got %+v", q1)
	}
}

func TestSupersede(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d", time.Now().UnixNano())
	now := time.Now().Unix()

	v1 := []MemoryIn{{Title: "Daemon port decision", Content: "The daemon listens on 7690.", Kind: "decision"}}
	v2 := []MemoryIn{{Title: "Daemon port decision", Content: "The daemon listens on 7999 after the port clash.", Kind: "decision"}}
	if _, err := s.SaveBatch(ctx, pk, "sess-1", now, v1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBatch(ctx, pk, "sess-1", now+10, v2); err != nil {
		t.Fatal(err)
	}

	q1, _, _, err := s.Candidates(ctx, pk, []string{"daemon", "listens"})
	if err != nil {
		t.Fatal(err)
	}
	if len(q1) != 1 {
		t.Fatalf("superseded memory must be excluded from retrieval, got %d results", len(q1))
	}
	if q1[0].Content != v2[0].Content {
		t.Fatalf("surviving memory should be v2, got %q", q1[0].Content)
	}
}

func TestCursorRoundtrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d", time.Now().UnixNano())
	sid := fmt.Sprintf("sess-%d", time.Now().UnixNano())

	if c, err := s.GetCursor(ctx, sid); err != nil || c != 0 {
		t.Fatalf("unknown session cursor should be 0, got %d err=%v", c, err)
	}
	if err := s.SetCursor(ctx, sid, pk, 42, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if c, err := s.GetCursor(ctx, sid); err != nil || c != 42 {
		t.Fatalf("cursor roundtrip failed: %d err=%v", c, err)
	}
}

func TestEntityNames(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d", time.Now().UnixNano())
	if _, err := s.SaveBatch(ctx, pk, "sess-1", time.Now().Unix(), seedBatch()); err != nil {
		t.Fatal(err)
	}
	names, err := s.EntityNames(ctx, pk, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Fatalf("want 3 entities, got %v", names)
	}
}
