//go:build integration

package graph

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/Rampo0/infinite-memory/internal/backup"
	"github.com/Rampo0/infinite-memory/internal/config"
)

// Run with: make itest — it starts a throwaway Memgraph on 7688 first.
// Every test seeds /itest/<nanos> fixtures and never deletes them, which is
// why the live graph (config memgraph_uri, 7687 by default) is refused: those
// fixtures used to compete with real memories in every search. Assertions
// still filter candidates to the test's own pk.

// testBoltURI is IMEM_TEST_BOLT (default the make itest instance), never the
// live graph unless IMEM_TEST_ALLOW_LIVE=1.
func testBoltURI(t *testing.T) string {
	t.Helper()
	uri := os.Getenv("IMEM_TEST_BOLT")
	if uri == "" {
		uri = "bolt://127.0.0.1:7688"
	}
	if uri == config.Load().MemgraphURI && os.Getenv("IMEM_TEST_ALLOW_LIVE") != "1" {
		t.Fatalf("refusing to seed test fixtures into the live graph at %s (IMEM_TEST_ALLOW_LIVE=1 overrides)", uri)
	}
	return uri
}

func testStore(t *testing.T) *Store {
	t.Helper()
	uri := testBoltURI(t)
	s, err := New(uri, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// The test instance may still be booting right after make itest starts it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for err = s.Ping(ctx); err != nil && ctx.Err() == nil; err = s.Ping(ctx) {
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Skipf("test memgraph not reachable at %s (make itest starts it): %v", uri, err)
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
	if len(saved) != 2 {
		t.Fatalf("want 2 saved, got %d", len(saved))
	}
	for _, o := range saved {
		if !o.New || o.Seen != 1 {
			t.Fatalf("first write should be new with seen 1, got %+v", o)
		}
	}

	q1, q2, _, err := s.Candidates(ctx, []string{"daemon", "port"}, 1)
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
	_, q2b, _, err := s.Candidates(ctx, []string{"memgraph"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyPK(q2b, pk)) != 2 {
		t.Fatalf("memgraph entity should hit both seeded memories, got %d", len(onlyPK(q2b, pk)))
	}

	// Q3: 1-hop — "retrieval" relates to "memgraph" via co-occurrence, and
	// memgraph mentions the port memory, so expansion should reach it.
	_, _, q3b, err := s.Candidates(ctx, []string{"retrieval"}, 1)
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

	_, q2, _, err := s.Candidates(ctx, []string{fmt.Sprintf("zzz"), "jago", "whitelist"}, 1)
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
	q1, _, _, err := s.Candidates(ctx, []string{"listens"}, 1)
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

	q1, _, _, err := s.Candidates(ctx, []string{"daemon", "listens"}, 1)
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

// TestDump is read-only against the live graph: it never calls Restore, which
// would DROP GRAPH on the developer's real memories.
func TestDump(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stmts, err := s.Dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) == 0 {
		t.Fatal("dump is empty")
	}
	var constraints, cleanup int
	for _, q := range stmts {
		if strings.HasPrefix(q, "CREATE CONSTRAINT") {
			constraints++
		}
		if strings.Contains(q, "REMOVE u:__mg_vertex__") {
			cleanup++
		}
	}
	if constraints == 0 {
		t.Error("dump carries no CREATE CONSTRAINT statements, schema would be lost on restore")
	}
	if cleanup == 0 {
		t.Error("dump carries no __mg_vertex__ cleanup statement")
	}
}

// TestDumpIsReplayable is the regression guard for the bug that made the very
// first restore fail: Entity.key carries a NUL separator, DUMP DATABASE emits
// it raw, and Memgraph's own parser then rejects the statement. EXPLAIN parses
// a query without running it, so this validates every escaped statement
// against the live parser without touching the graph.
func TestDumpIsReplayable(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	stmts, err := s.Dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) == 0 {
		t.Skip("graph is empty, nothing to validate")
	}

	var checked, withEscapes int
	for _, q := range stmts {
		// EXPLAIN cannot parse DDL, and the NUL bug lives in data statements.
		// Against the big live graph the 50-statement slice never reached the
		// schema tail; the small isolated test graph does.
		if isSchemaStatement(q) {
			continue
		}
		esc := backup.EscapeControl(q)
		// Every statement carrying a control byte is a candidate for the bug;
		// a slice of the rest keeps the test honest without 2000 round trips.
		if esc == q && checked >= 50 {
			continue
		}
		if esc != q {
			withEscapes++
		}
		checked++
		if err := s.explain(ctx, esc); err != nil {
			t.Fatalf("statement is not replayable: %v\nquery: %.200q", err, esc)
		}
	}
	t.Logf("validated %d/%d statements (%d needed escaping)", checked, len(stmts), withEscapes)
	if withEscapes == 0 {
		t.Log("note: no control bytes in this graph, the NUL path went unexercised")
	}
}

// isSchemaStatement reports DUMP DATABASE's index/constraint statements.
func isSchemaStatement(q string) bool {
	u := strings.ToUpper(strings.TrimSpace(q))
	for _, p := range []string{"CREATE INDEX", "CREATE EDGE INDEX", "CREATE TEXT INDEX", "CREATE POINT INDEX",
		"CREATE VECTOR INDEX", "CREATE CONSTRAINT", "DROP INDEX", "DROP CONSTRAINT", "DROP EDGE INDEX"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// The retriever's idf needs to know WHICH terms and entities each memory
// matched, not only how many.
func TestCandidatesReturnMatchedTerms(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-matched", time.Now().UnixNano())
	if _, err := s.SaveBatch(ctx, pk, "sess-m", time.Now().Unix(), seedBatch()); err != nil {
		t.Fatal(err)
	}
	q1, q2, _, err := s.Candidates(ctx, []string{"daemon", "port", "nothingmatches"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	var kw []string
	for _, c := range onlyPK(q1, pk) {
		if c.Title == "Daemon port decision" {
			kw = c.Matched
		}
	}
	if strings.Join(kw, ",") != "daemon,port" {
		t.Fatalf("keyword matched terms: want [daemon port], got %v", kw)
	}
	var exact, part []string
	for _, c := range onlyPK(q2, pk) {
		if c.Title == "Daemon port decision" {
			exact, part = c.Matched, c.Partial
		}
	}
	// "daemon" is only a token of "imem daemon": partial, not an exact name.
	if len(exact) != 0 || strings.Join(part, ",") != "daemon" {
		t.Fatalf("want no exact entity and partial [daemon], got exact %v partial %v", exact, part)
	}
	_, q2x, _, err := s.Candidates(ctx, []string{"imem daemon"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	exact = nil
	for _, c := range onlyPK(q2x, pk) {
		if c.Title == "Daemon port decision" {
			exact = c.Matched
		}
	}
	if strings.Join(exact, ",") != "imem daemon" {
		t.Fatalf("a whole entity name is an exact match, got %v", exact)
	}
}

// RELATED edges below minRel are not followed: one chance co-occurrence is
// not a relation worth widening a search for.
func TestRelatedHopRespectsMinWeight(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-relw", time.Now().UnixNano())
	now := time.Now().Unix()
	pair := []EntityIn{{Name: "zqx seed topic", Type: "topic"}, {Name: "zqx neighbour", Type: "topic"}}
	neighbourOnly := MemoryIn{Title: "Neighbour only", Content: "Mentions only the zqx neighbour.", Kind: "fact",
		Entities: []EntityIn{{Name: "zqx neighbour", Type: "topic"}}}
	once := MemoryIn{Title: "Pair once", Content: "First co-occurrence of the zqx pair.", Kind: "fact", Entities: pair}
	if _, err := s.SaveBatch(ctx, pk, "sess-r", now, []MemoryIn{neighbourOnly, once}); err != nil {
		t.Fatal(err)
	}
	reach := func(minRel int) bool {
		_, _, q3, err := s.Candidates(ctx, []string{"zqx seed topic"}, minRel)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range onlyPK(q3, pk) {
			if c.Title == "Neighbour only" {
				return true
			}
		}
		return false
	}
	if !reach(1) || reach(2) {
		t.Fatalf("a weight-1 edge: followed at minRel 1 (%v), not at 2 (%v)", reach(1), reach(2))
	}
	twice := MemoryIn{Title: "Pair twice", Content: "Second co-occurrence of the zqx pair.", Kind: "fact", Entities: pair}
	if _, err := s.SaveBatch(ctx, pk, "sess-r", now, []MemoryIn{twice}); err != nil {
		t.Fatal(err)
	}
	if !reach(2) {
		t.Fatal("a weight-2 edge must be followed at minRel 2")
	}
}

func TestCountLiveSkipsSuperseded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	before, err := s.CountLive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pk := fmt.Sprintf("/itest/%d-count", time.Now().UnixNano())
	now := time.Now().Unix()
	v1 := MemoryIn{Title: "Count me", Content: "first version", Kind: "fact"}
	v2 := MemoryIn{Title: "Count me", Content: "second version supersedes the first", Kind: "fact"}
	if _, err := s.SaveBatch(ctx, pk, "sess-c", now, []MemoryIn{v1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBatch(ctx, pk, "sess-c", now, []MemoryIn{v2}); err != nil {
		t.Fatal(err)
	}
	after, err := s.CountLive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after-before != 1 {
		t.Fatalf("two versions, one superseded: live count should grow by 1, grew by %d", after-before)
	}
}

func TestByKindPreferences(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-kind", time.Now().UnixNano())
	pref := MemoryIn{Title: "Prefers stdlib zqk", Content: "User prefers stdlib over frameworks zqk.", Kind: "preference"}
	fact := MemoryIn{Title: "Some fact zqk", Content: "A plain fact zqk.", Kind: "fact"}
	if _, err := s.SaveBatch(ctx, pk, "sess-k", time.Now().Unix(), []MemoryIn{pref, fact}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ByKind(ctx, "preference", -1)
	if err != nil {
		t.Fatal(err)
	}
	mine := onlyPK(got, pk)
	if len(mine) != 1 || mine[0].Title != "Prefers stdlib zqk" || mine[0].Kind != "preference" {
		t.Fatalf("want only the preference, got %+v", mine)
	}
}

// Aliases are searchable keywords even on a long memory whose own text
// already fills the keyword cap, and a re-observation adds new ones.
func TestSaveIndexesAliases(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-alias", time.Now().UnixNano())
	now := time.Now().Unix()
	long := strings.Repeat("filler word zq ", 0)
	for i := 0; i < 60; i++ {
		long += fmt.Sprintf("word%02d ", i)
	}
	m := MemoryIn{Title: "BCA RDN SFTP switch", Content: long, Kind: "fact", Aliases: []string{"bcardnzq", "rekening dana zq"}}
	if _, err := s.SaveBatch(ctx, pk, "sess-a", now, []MemoryIn{m}); err != nil {
		t.Fatal(err)
	}
	found := func(tok string) bool {
		q1, _, _, err := s.Candidates(ctx, []string{tok}, 1)
		if err != nil {
			t.Fatal(err)
		}
		return len(onlyPK(q1, pk)) == 1
	}
	if !found("bcardnzq") || !found("rekening") {
		t.Fatal("alias tokens must be indexed even past the content's keyword cap")
	}
	m.Aliases = []string{"lambatzq"}
	if _, err := s.SaveBatch(ctx, pk, "sess-a", now, []MemoryIn{m}); err != nil {
		t.Fatal(err)
	}
	if !found("lambatzq") || !found("bcardnzq") {
		t.Fatal("a re-observation must add its new aliases and keep the old ones")
	}
}

// Backfill targets memories that were never aliased (aliases IS NULL — older
// than the alias extractor), and SetAliases both stores and indexes them.
func TestAliasBackfillQueries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Not under /itest/: the backfill skips that prefix (live-graph fixtures).
	pk := fmt.Sprintf("/aliastest/%d", time.Now().UnixNano())
	if _, err := s.SaveBatch(ctx, pk, "sess-b", time.Now().Unix(), []MemoryIn{
		{Title: "Old memory zqb", Content: "Saved before aliases existed zqb.", Kind: "fact"},
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-alias memory: the property is absent, not empty.
	if err := s.write(ctx, "MATCH (m:Memory {project_key: $pk}) REMOVE m.aliases", map[string]any{"pk": pk}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.MemoriesWithoutAliases(ctx, 5000)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, p := range pending {
		if p.Title == "Old memory zqb" {
			id = p.ID
		}
	}
	if id == "" {
		t.Fatal("a memory without aliases must be pending")
	}
	if err := s.SetAliases(ctx, id, []string{"memori lama zqb"}); err != nil {
		t.Fatal(err)
	}
	q1, _, _, err := s.Candidates(ctx, []string{"lama"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyPK(q1, pk)) != 1 {
		t.Fatal("SetAliases must index the alias tokens as keywords")
	}
	pending, _ = s.MemoriesWithoutAliases(ctx, 5000)
	for _, p := range pending {
		if p.ID == id {
			t.Fatal("an aliased memory is no longer pending")
		}
	}
}

func liveByTitle(t *testing.T, s *Store, pk, title string) (id string, superseded bool, seen int64) {
	t.Helper()
	sess := s.driver.NewSession(context.Background(), neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(context.Background())
	res, err := sess.Run(context.Background(),
		"MATCH (m:Memory {project_key: $pk, title: $title}) RETURN m.id AS id, coalesce(m.superseded,false) AS sup, m.seen_count AS seen",
		map[string]any{"pk": pk, "title": title})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Next(context.Background()) {
		return "", false, 0
	}
	rec := res.Record()
	sup, _ := rec.Get("sup")
	return recStr(rec, "id"), sup.(bool), recInt(rec, "seen")
}

// Reconcile "update": the new memory retires the one it replaces, by id,
// even when the titles differ (the title-based supersede would miss it).
func TestSaveBatchUpdateSupersedesTarget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-upd", time.Now().UnixNano())
	now := time.Now().Unix()
	if _, err := s.SaveBatch(ctx, pk, "sess-u", now, []MemoryIn{{Title: "Expansion runs live per prompt zqu", Content: "Old decision zqu.", Kind: "decision"}}); err != nil {
		t.Fatal(err)
	}
	oldID, _, _ := liveByTitle(t, s, pk, "Expansion runs live per prompt zqu")
	out, err := s.SaveBatch(ctx, pk, "sess-u", now, []MemoryIn{{Title: "Expansion moved to index time zqu",
		Content: "New decision zqu.", Kind: "decision", Op: "update", TargetID: oldID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, sup, _ := liveByTitle(t, s, pk, "Expansion runs live per prompt zqu"); !sup {
		t.Fatal("the replaced memory must be superseded")
	}
	if len(out) != 1 || !out[0].New || !out[0].Updated {
		t.Fatalf("outcome must say new and updated: %+v", out)
	}
}

// Reconcile "noop": the existing memory is re-observed; nothing new is saved.
func TestSaveBatchNoopReinforcesTarget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-noop", time.Now().UnixNano())
	now := time.Now().Unix()
	if _, err := s.SaveBatch(ctx, pk, "sess-n", now, []MemoryIn{{Title: "Daemon port zqn", Content: "7690 zqn.", Kind: "fact"}}); err != nil {
		t.Fatal(err)
	}
	id, _, _ := liveByTitle(t, s, pk, "Daemon port zqn")
	out, err := s.SaveBatch(ctx, pk, "sess-n", now+60, []MemoryIn{{Title: "Port again", Content: "brief", Kind: "fact", Op: "noop", TargetID: id}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, seen := liveByTitle(t, s, pk, "Daemon port zqn"); seen != 2 {
		t.Fatalf("noop must re-observe the target: seen %d", seen)
	}
	if other, _, _ := liveByTitle(t, s, pk, "Port again"); other != "" {
		t.Fatal("noop must not save a new memory")
	}
	if len(out) != 1 || out[0].ID != id || out[0].New || out[0].Seen != 2 || out[0].Title != "Daemon port zqn" {
		t.Fatalf("outcome must describe the reinforced target: %+v", out)
	}
}

// B2: feedback counters land on the memories and come back with candidates.
func TestFeedbackCounters(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-fb", time.Now().UnixNano())
	now := time.Now().Unix()
	if _, err := s.SaveBatch(ctx, pk, "sess-f", now, []MemoryIn{{Title: "Feedback target zqf", Content: "Graded memory zqf.", Kind: "fact"}}); err != nil {
		t.Fatal(err)
	}
	id, _, _ := liveByTitle(t, s, pk, "Feedback target zqf")
	if err := s.ApplyFeedback(ctx, now+100, []Verdict{{ID: id, Verdict: "used"}, {ID: id, Verdict: "used"}, {ID: id, Verdict: "outdated"}, {ID: "nope", Verdict: "used"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkInjected(ctx, []string{id, id}, now+50); err != nil {
		t.Fatal(err)
	}
	q1, _, _, err := s.Candidates(ctx, []string{"zqf"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	got := onlyPK(q1, pk)
	if len(got) != 1 || got[0].UsedCount != 2 || got[0].DisputedCount != 1 || got[0].LastUsed != now+100 || got[0].InjectedCount != 2 {
		t.Fatalf("want used 2, disputed 1, last used now+100, injected 2: %+v", got)
	}
}

// C1: an old worktree-keyed memory, once given a repo_key, reports the repo
// as its project everywhere retrieval looks — without touching its key/hash.
func TestRepoKeyOverridesProjectKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := fmt.Sprintf("/itest/%d-repo", time.Now().UnixNano())
	wt := base + "/.superset/worktrees/abc/feature-x"
	repo := base + "/registration"
	if _, err := s.SaveBatch(ctx, wt, "sess-r", time.Now().Unix(), []MemoryIn{
		{Title: "Worktree fact zqr", Content: "Saved from a worktree zqr.", Kind: "fact"},
		{Title: "Worktree rule zqr", Content: "A rule from a worktree zqr.", Kind: "rule"},
	}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ProjectKeys(ctx)
	if err != nil || !slices.Contains(keys, wt) {
		t.Fatalf("project keys must list the worktree key: %v %v", keys, err)
	}
	n, err := s.SetRepoKey(ctx, wt, repo)
	if err != nil || n != 2 {
		t.Fatalf("want 2 memories re-keyed, got %d %v", n, err)
	}
	q1, _, _, err := s.Candidates(ctx, []string{"zqr"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyPK(q1, repo)) != 2 || len(onlyPK(q1, wt)) != 0 {
		t.Fatalf("candidates must report the repo key: %+v", q1)
	}
	rules, _ := s.ByKind(ctx, "rule", -1)
	if len(onlyPK(rules, repo)) != 1 {
		t.Fatal("rules must report the repo key too")
	}
}

func TestSupersedeByKeepsTheKeeper(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-cons", time.Now().UnixNano())
	now := time.Now().Unix()
	out, err := s.SaveBatch(ctx, pk, "sess-c", now, []MemoryIn{
		{Title: "Dup one zqc", Content: "ztauth verification race zqc one.", Kind: "fact", Entities: []EntityIn{{Name: "ztauth zqc"}}},
		{Title: "Dup two zqc", Content: "ztauth verification race zqc two.", Kind: "fact", Entities: []EntityIn{{Name: "ztauth zqc"}}},
		{Title: "Canonical zqc", Content: "ztauth verification: race tests zqc.", Kind: "fact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.SupersedeBy(ctx, out[2].ID, []string{out[0].ID, out[1].ID, out[2].ID})
	if err != nil || n != 2 {
		t.Fatalf("want the 2 others superseded, got %d %v", n, err)
	}
	if _, sup, _ := liveByTitle(t, s, pk, "Canonical zqc"); sup {
		t.Fatal("the keeper must stay live")
	}
	if _, sup, _ := liveByTitle(t, s, pk, "Dup one zqc"); !sup {
		t.Fatal("members must be superseded")
	}
	live, err := s.LiveMemories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range live {
		if m.ProjectKey == pk && m.Title != "Canonical zqc" {
			t.Fatalf("superseded memories are not live: %+v", m)
		}
	}
}

func TestLiveMemoriesCarryKeywordsAndEntities(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/cons-test/%d", time.Now().UnixNano()) // /itest/ is skipped by LiveMemories
	if _, err := s.SaveBatch(ctx, pk, "sess-l", time.Now().Unix(), []MemoryIn{
		{Title: "Jago whitelist zql", Content: "Rows live in master data zql.", Kind: "fact", Entities: []EntityIn{{Name: "Jago Whitelist"}}},
	}); err != nil {
		t.Fatal(err)
	}
	live, err := s.LiveMemories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range live {
		if m.ProjectKey == pk {
			if !slices.Contains(m.Keywords, "zql") || !slices.Contains(m.Entities, "jago whitelist") {
				t.Fatalf("want keywords and lowercased entity names: %+v", m)
			}
			return
		}
	}
	t.Fatal("the memory must be listed")
}

// C2 archive: a memory that kept being injected but was never used goes out
// of retrieval (not deleted).
func TestArchiveTakesMemoriesOutOfRetrieval(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-arch", time.Now().UnixNano())
	old := time.Now().Unix() - 90*86400
	out, err := s.SaveBatch(ctx, pk, "sess-a", old, []MemoryIn{
		{Title: "Never used zqa", Content: "Injected a lot zqa.", Kind: "rule"},
		{Title: "Used zqa", Content: "Injected and used zqa.", Kind: "fact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{out[0].ID, out[1].ID}
	for i := 0; i < 25; i++ {
		if err := s.MarkInjected(ctx, ids, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ApplyFeedback(ctx, old, []Verdict{{ID: out[1].ID, Verdict: "used"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.CountLive(ctx)
	cands, err := s.ArchiveCandidates(ctx, 20, time.Now().Unix()-60*86400)
	if err != nil {
		t.Fatal(err)
	}
	var mine []string
	for _, c := range cands {
		if c.ID == out[0].ID || c.ID == out[1].ID {
			mine = append(mine, c.Title)
		}
	}
	if strings.Join(mine, ",") != "Never used zqa" {
		t.Fatalf("only the never-used memory is a candidate, got %v", mine)
	}
	if err := s.Archive(ctx, []string{out[0].ID}); err != nil {
		t.Fatal(err)
	}
	q1, _, _, _ := s.Candidates(ctx, []string{"zqa"}, 1)
	rules, _ := s.ByKind(ctx, "rule", -1)
	after, _ := s.CountLive(ctx)
	if len(onlyPK(q1, pk)) != 1 || len(onlyPK(rules, pk)) != 0 || after != before-1 {
		t.Fatalf("an archived memory leaves search, rules and the corpus count: q1 %d rules %d count %d->%d",
			len(onlyPK(q1, pk)), len(onlyPK(rules, pk)), before, after)
	}
}

// alias_only keeps the alias tokens the memory's own text lacks, so retrieval
// can tell a guess from the memory's words.
func TestAliasOnlyTokensAreTracked(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-ao", time.Now().UnixNano())
	now := time.Now().Unix()
	if _, err := s.SaveBatch(ctx, pk, "sess-ao", now, []MemoryIn{{Title: "Field mask guard zqo",
		Content: "Empty field mask returns invalid parameter zqo.", Kind: "decision", Aliases: []string{"wajibzq diisizq", "parameter"}}}); err != nil {
		t.Fatal(err)
	}
	via := func(tok string) []string {
		q1, _, _, err := s.Candidates(ctx, []string{tok}, 1)
		if err != nil {
			t.Fatal(err)
		}
		mine := onlyPK(q1, pk)
		if len(mine) != 1 {
			t.Fatalf("%s: want the memory, got %d", tok, len(mine))
		}
		return mine[0].ViaAlias
	}
	if got := via("wajibzq"); strings.Join(got, ",") != "wajibzq" {
		t.Fatalf("an alias-only token must be reported as such, got %v", got)
	}
	if got := via("parameter"); len(got) != 0 {
		t.Fatalf("a token in the memory's own text is not alias-only, got %v", got)
	}

	// Backfill path: SetAliases marks only the new tokens as alias-only.
	if _, err := s.SaveBatch(ctx, pk, "sess-ao", now, []MemoryIn{{Title: "Old memory zqo2", Content: "Plain field text zqo2.", Kind: "fact"}}); err != nil {
		t.Fatal(err)
	}
	id, _, _ := liveByTitle(t, s, pk, "Old memory zqo2")
	if err := s.SetAliases(ctx, id, []string{"lambatzq", "field"}); err != nil {
		t.Fatal(err)
	}
	q1, _, _, _ := s.Candidates(ctx, []string{"lambatzq", "field"}, 1)
	for _, c := range onlyPK(q1, pk) {
		if c.Title == "Old memory zqo2" && strings.Join(c.ViaAlias, ",") != "lambatzq" {
			t.Fatalf("backfilled alias-only tokens: want [lambatzq], got %v", c.ViaAlias)
		}
	}
}

func TestReindexRewritesTokenLists(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/reindex-test/%d", time.Now().UnixNano())
	if _, err := s.SaveBatch(ctx, pk, "sess-ri", time.Now().Unix(), []MemoryIn{{Title: "Reindex me zqr2", Content: "Some text zqr2.", Kind: "fact"}}); err != nil {
		t.Fatal(err)
	}
	all, err := s.IndexRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var row IndexRow
	for _, r := range all {
		if r.Title == "Reindex me zqr2" {
			row = r
		}
	}
	if row.ID == "" || !slices.Contains(row.Keywords, "zqr2") {
		t.Fatalf("index rows must carry the stored keywords: %+v", row)
	}
	if err := s.SetIndex(ctx, row.ID, []string{"fresh", "zqr2"}, []string{"fresh"}); err != nil {
		t.Fatal(err)
	}
	q1, _, _, _ := s.Candidates(ctx, []string{"fresh"}, 1)
	if len(onlyPK(q1, pk)) != 1 || strings.Join(onlyPK(q1, pk)[0].ViaAlias, ",") != "fresh" {
		t.Fatalf("reindexed lists must drive retrieval: %+v", onlyPK(q1, pk))
	}
}

func TestPinRulesAndPreferencesOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d-pins", time.Now().UnixNano())
	mems := []MemoryIn{
		{Title: "Pinned constitution " + pk, Content: "Functions under 60 lines.", Kind: "rule"},
		{Title: "Pinned fact " + pk, Content: "The daemon listens on 7690.", Kind: "fact"},
	}
	if _, err := s.SaveBatch(ctx, pk, "sess-pin", time.Now().Unix(), mems); err != nil {
		t.Fatal(err)
	}
	targets, err := s.PinTargets(ctx, "pinned constitution "+pk)
	if err != nil || len(targets) != 1 || targets[0].Kind != "rule" || targets[0].Pinned {
		t.Fatalf("want one unpinned rule target, got %+v (%v)", targets, err)
	}
	if facts, _ := s.PinTargets(ctx, "pinned fact "+pk); len(facts) != 0 {
		t.Fatalf("facts are not pinnable, got %+v", facts)
	}
	if err := s.SetPinned(ctx, targets[0].ID, true); err != nil {
		t.Fatal(err)
	}
	rules, err := s.Rules(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range onlyPK(rules, pk) {
		if r.ID == targets[0].ID && !r.Pinned {
			t.Fatalf("pin did not stick: %+v", r)
		}
	}
	if again, _ := s.PinTargets(ctx, targets[0].ID); len(again) != 1 || !again[0].Pinned {
		t.Fatalf("an exact id must find the pinned rule, got %+v", again)
	}
}

func TestTouchSessionFeedsTheSweep(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d", time.Now().UnixNano())
	sid := fmt.Sprintf("sweep-%d", time.Now().UnixNano())
	now := time.Now().Unix()
	src := SessionSource{ID: sid, ProjectKey: pk, TranscriptPath: "/tmp/" + sid + ".jsonl", CWD: "/c", Agent: true}
	if err := s.TouchSession(ctx, src, now); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSession(ctx, SessionSource{ID: sid, ProjectKey: pk, TranscriptPath: src.TranscriptPath, CWD: "/c"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCursor(ctx, sid, pk, 7, now); err != nil {
		t.Fatal(err)
	}
	cands, err := s.SweepCandidates(ctx, now-1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.ID == sid {
			if c.TranscriptPath != src.TranscriptPath || !c.Agent || c.Cursor != 7 || c.UpdatedAt != now {
				t.Fatalf("sweep candidate misread (agent must stay sticky): %+v", c)
			}
			return
		}
	}
	t.Fatalf("touched session %s missing from the sweep", sid)
}

func TestLearnAliasesAddsSearchableTermsUpToACap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d", time.Now().UnixNano())
	saved, err := s.SaveBatch(ctx, pk, "learn-"+pk, time.Now().Unix(), []MemoryIn{{
		Title: "BCA RDN creation", Content: "Callback rejected when the signature is stale.", Kind: "fact"}})
	if err != nil || len(saved) != 1 {
		t.Fatalf("seed: %v %v", saved, err)
	}
	id := saved[0].ID
	if err := s.LearnAliases(ctx, id, []string{"rekening", "nasabah"}); err != nil {
		t.Fatal(err)
	}
	if err := s.LearnAliases(ctx, id, []string{"rekening", "dana"}); err != nil {
		t.Fatal(err)
	}
	q1, _, _, err := s.Candidates(ctx, []string{"nasabah"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range onlyPK(q1, pk) {
		found = found || c.ID == id
	}
	if !found {
		t.Fatal("a learned alias must make the memory findable by that word")
	}
}

func TestMemoryTargetsFindAnyKindByTitleWords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pk := fmt.Sprintf("/itest/%d", time.Now().UnixNano())
	saved, err := s.SaveBatch(ctx, pk, "dispute-"+pk, time.Now().Unix(), []MemoryIn{{
		Title: "Rules file verified in restricted sessions " + pk, Content: "Wrong claim.", Kind: "decision"}})
	if err != nil || len(saved) != 1 {
		t.Fatalf("seed: %v %v", saved, err)
	}
	got, err := s.MemoryTargets(ctx, "verified in restricted sessions "+pk)
	if err != nil || len(got) != 1 || got[0].ID != saved[0].ID || got[0].Kind != "decision" {
		t.Fatalf("a decision must be findable by its title: %+v %v", got, err)
	}
}
