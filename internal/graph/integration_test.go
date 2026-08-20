//go:build integration

package graph

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Requires a running Memgraph (make up). Run with: make itest
// Retrieval is global, so assertions filter candidates to this test's pk —
// the live database may hold real memories that also match.

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

func onlyPK(cands []Candidate, pk string) []Candidate {
	var out []Candidate
	for _, c := range cands {
		if c.ProjectKey == pk {
			out = append(out, c)
		}
	}
	return out
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

	q1, q2, _, err := s.Candidates(ctx, []string{"daemon", "port"})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyPK(q1, pk)) == 0 {
		t.Fatalf("keyword query found nothing for pk")
	}
	if len(onlyPK(q2, pk)) == 0 {
		t.Fatalf("entity query found nothing (entity tokens should match 'daemon')")
	}

	// Q2 exact entity name, scoped to this pk.
	_, q2b, _, err := s.Candidates(ctx, []string{"memgraph"})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyPK(q2b, pk)) != 2 {
		t.Fatalf("memgraph entity should hit both seeded memories, got %d", len(onlyPK(q2b, pk)))
	}

	// Q3: 1-hop — "retrieval" relates to "memgraph" via co-occurrence, and
	// memgraph mentions the port memory, so expansion should reach it.
	_, _, q3b, err := s.Candidates(ctx, []string{"retrieval"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range onlyPK(q3b, pk) {
		if c.Title == "Daemon port decision" {
			found = true
		}
	}
	if !found {
		t.Fatalf("1-hop expansion should surface the port memory")
	}
}

func TestGlobalRetrievalAcrossProjects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	pkA := fmt.Sprintf("/itest/%d-repoA", suffix)
	pkB := fmt.Sprintf("/itest/%d-repoB", suffix)
	now := time.Now().Unix()

	topic := fmt.Sprintf("zzz jago whitelist %d", suffix)
	mem := []MemoryIn{{
		Title: "Jago whitelist flow", Content: "Whitelist entries validated against the " + topic + " service.",
		Kind: "fact", Entities: []EntityIn{{Name: topic, Type: "topic"}},
	}}
	if _, err := s.SaveBatch(ctx, pkA, "sess-A", now, mem); err != nil {
		t.Fatal(err)
	}
	_ = pkB // the query below simulates asking from repoB: no pk filter anywhere

	_, q2, _, err := s.Candidates(ctx, []string{fmt.Sprintf("zzz"), "jago", "whitelist"})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyPK(q2, pkA)) == 0 {
		t.Fatalf("topic memory from repoA must be retrievable without its pk")
	}
}

func TestRulesQuery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-rules", time.Now().UnixNano())
	now := time.Now().Unix()

	rules := []MemoryIn{
		{Title: "Function length limit", Content: "Functions stay under 60 lines.", Kind: "rule"},
		{Title: "Max function args", Content: "Functions take at most 4 arguments.", Kind: "rule"},
	}
	if _, err := s.SaveBatch(ctx, pk, "sess-r", now, rules); err != nil {
		t.Fatal(err)
	}
	got, err := s.Rules(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyPK(got, pk)) != 2 {
		t.Fatalf("want 2 rules for pk, got %d", len(onlyPK(got, pk)))
	}
	for _, r := range onlyPK(got, pk) {
		if r.Kind != "rule" {
			t.Fatalf("non-rule kind in rules query: %+v", r)
		}
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
	q1, _, _, err := s.Candidates(ctx, []string{"listens"})
	if err != nil {
		t.Fatal(err)
	}
	mine := onlyPK(q1, pk)
	if len(mine) != 1 || mine[0].SeenCount != 2 {
		t.Fatalf("duplicate save should bump seen_count to 2, got %+v", mine)
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

	q1, _, _, err := s.Candidates(ctx, []string{"daemon", "listens"})
	if err != nil {
		t.Fatal(err)
	}
	mine := onlyPK(q1, pk)
	if len(mine) != 1 {
		t.Fatalf("superseded memory must be excluded from retrieval, got %d results", len(mine))
	}
	if mine[0].Content != v2[0].Content {
		t.Fatalf("surviving memory should be v2, got %q", mine[0].Content)
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

func TestEntityListAndDetail(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-ent", time.Now().UnixNano())
	if _, err := s.SaveBatch(ctx, pk, "sess-e", time.Now().Unix(), seedBatch()); err != nil {
		t.Fatal(err)
	}

	list, err := s.EntityList(ctx, pk, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 entities for pk, got %d", len(list))
	}
	if list[0].Name != "Memgraph" || list[0].Mentions != 2 {
		t.Fatalf("Memgraph (2 mentions) should rank first, got %+v", list[0])
	}

	d, err := s.EntityDetail(ctx, "imem daemon")
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyPKInfo(d.Nodes, pk)) == 0 {
		t.Fatalf("detail should find the seeded node, got %+v", d.Nodes)
	}
	foundRel, foundVerb := false, false
	for _, r := range d.Relations {
		if r.Name == "Memgraph" && r.ProjectKey == pk {
			foundRel = true
			if r.Verb == "uses" {
				foundVerb = true
			}
		}
	}
	if !foundRel || !foundVerb {
		t.Fatalf("relation 'uses Memgraph' missing: %+v", d.Relations)
	}
	foundMem := false
	for _, m := range d.Memories {
		if m.ProjectKey == pk && m.Title == "Daemon port decision" {
			foundMem = true
		}
	}
	if !foundMem {
		t.Fatalf("mentioning memory missing: %+v", d.Memories)
	}

	// substring fallback
	d2, err := s.EntityDetail(ctx, "imem daem")
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.Nodes) == 0 {
		t.Fatal("substring fallback found nothing")
	}
}

func onlyPKInfo(infos []EntityInfo, pk string) []EntityInfo {
	var out []EntityInfo
	for _, e := range infos {
		if e.ProjectKey == pk {
			out = append(out, e)
		}
	}
	return out
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
	global, err := s.EntityNamesGlobal(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(global) == 0 {
		t.Fatal("global entity names empty")
	}
}
