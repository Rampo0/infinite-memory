package consolidate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func mem(id, kind string, ents []string, kw ...string) Mem {
	return Mem{ID: id, Title: "T " + id, Content: "C " + id, Kind: kind, ProjectKey: "/r", Entities: ents, Keywords: kw}
}

func ids(c []Mem) string {
	var out []string
	for _, m := range c {
		out = append(out, m.ID)
	}
	return strings.Join(out, ",")
}

// Near-duplicates share an entity AND most of their words; sharing only a
// topic is not enough.
func TestClustersNeedEntityAndOverlap(t *testing.T) {
	mems := []Mem{
		mem("a", "fact", []string{"ztauth"}, "race", "coverage", "lint", "ztauth"),
		mem("b", "fact", []string{"ztauth"}, "race", "coverage", "lint", "configs"),
		mem("c", "fact", []string{"ztauth"}, "headers", "identity", "ambiguous", "reject"),
		mem("d", "fact", []string{"other"}, "race", "coverage", "lint", "ztauth"), // same words, no shared entity
	}
	got := Clusters(mems, 0.4, 30, 8)
	if len(got) != 1 || ids(got[0]) != "a,b" {
		t.Fatalf("want only a,b clustered, got %v", got)
	}
}

// Very common entities ("account") are topics, not duplicate signals; huge
// clusters are topics too.
func TestClustersSkipBroadGroups(t *testing.T) {
	var mems []Mem
	for i := 0; i < 5; i++ {
		mems = append(mems, mem(string(rune('a'+i)), "fact", []string{"account"}, "same", "words", "here"))
	}
	if got := Clusters(mems, 0.4, 4, 8); len(got) != 0 {
		t.Fatalf("an entity shared by more than maxGroup memories must be skipped, got %v", got)
	}
	if got := Clusters(mems, 0.4, 30, 3); len(got) != 0 {
		t.Fatalf("a cluster larger than maxSize must be dropped, got %v", got)
	}
}

func TestParseMergesWhitelistsAndBoundsKinds(t *testing.T) {
	cluster := []Mem{mem("a", "fact", nil), mem("b", "fact", nil), mem("c", "rule", nil)}
	raw := `{"merges":[` +
		`{"ids":["a","b"],"title":"Merged","content":"One canonical memory.","kind":"fact"},` +
		`{"ids":["a","c"],"title":"Reuses a","content":"x","kind":"rule"},` +
		`{"ids":["b","zz"],"title":"Invented id","content":"x","kind":"fact"},` +
		`{"ids":["c"],"title":"Single","content":"x","kind":"rule"},` +
		`{"ids":["a","b"],"title":"Escalates","content":"x","kind":"rule"}]}`
	got, err := Parse(raw, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || strings.Join(got[0].IDs, ",") != "a,b" || got[0].Title != "Merged" || got[0].Kind != "fact" {
		t.Fatalf("only the first valid, disjoint, non-escalating merge survives, got %+v", got)
	}
}

func TestProcessAppliesMerges(t *testing.T) {
	cluster := []Mem{mem("a", "fact", nil), mem("b", "fact", nil)}
	var applied []Merge
	c := Consolidator{
		Run: func(_ context.Context, prompt, schema, sys string) (string, error) {
			if !strings.Contains(prompt, "id=a") || !strings.Contains(prompt, "DATA") {
				t.Fatalf("prompt must list members as data:\n%s", prompt)
			}
			return `{"merges":[{"ids":["a","b"],"title":"M","content":"Merged.","kind":"fact"}]}`, nil
		},
		Apply: func(_ context.Context, m Merge, members []Mem) error {
			applied = append(applied, m)
			if ids(members) != "a,b" {
				t.Fatalf("Apply gets the merged members, got %s", ids(members))
			}
			return nil
		},
	}
	got, err := c.Process(context.Background(), cluster)
	if err != nil || len(got) != 1 || len(applied) != 1 {
		t.Fatalf("want one merge applied, got %v %v %v", got, applied, err)
	}
}

// Without Apply it only plans (imem consolidate --plan).
func TestProcessPlanOnly(t *testing.T) {
	c := Consolidator{Run: func(context.Context, string, string, string) (string, error) {
		return `{"merges":[{"ids":["a","b"],"title":"M","content":"Merged.","kind":"fact"}]}`, nil
	}}
	got, err := c.Process(context.Background(), []Mem{mem("a", "fact", nil), mem("b", "fact", nil)})
	if err != nil || len(got) != 1 {
		t.Fatalf("plan-only must still return the merges: %v %v", got, err)
	}
}

func TestProcessSpawnFailure(t *testing.T) {
	c := Consolidator{Run: func(context.Context, string, string, string) (string, error) { return "", errors.New("killed") }}
	if _, err := c.Process(context.Background(), []Mem{mem("a", "fact", nil), mem("b", "fact", nil)}); err == nil {
		t.Fatal("a failed spawn is an error")
	}
}

// The canonical memory lands in the members' most common project, keeps
// their entities and aliases (bounded), and retires the members.
func TestCanonicalMemory(t *testing.T) {
	members := []Mem{
		{ID: "a", ProjectKey: "/repo", Entities: []string{"ztauth", "lint"}, Aliases: []string{"verifikasi", "ztauth"}},
		{ID: "b", ProjectKey: "/repo", Entities: []string{"ztauth"}, Aliases: []string{"race"}},
		{ID: "c", ProjectKey: "/worktree", Entities: []string{"coverage"}},
	}
	pk, m := Canonical(Merge{IDs: []string{"a", "b", "c"}, Title: "T", Content: "C", Kind: "rule"}, members)
	if pk != "/repo" || m.Title != "T" || m.Kind != "rule" {
		t.Fatalf("want the majority project and the merge's text: %s %+v", pk, m)
	}
	var ents []string
	for _, e := range m.Entities {
		ents = append(ents, e.Name)
	}
	if strings.Join(ents, ",") != "ztauth,lint,coverage" || strings.Join(m.Aliases, ",") != "verifikasi,ztauth,race" {
		t.Fatalf("entities and aliases are unioned in order: %v %v", ents, m.Aliases)
	}
}
