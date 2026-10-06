package aliases

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func items(ids ...string) []Item {
	out := make([]Item, len(ids))
	for i, id := range ids {
		out[i] = Item{ID: id, Title: "Title " + id, Content: "Content " + id, Kind: "fact"}
	}
	return out
}

func TestBuildPromptCarriesItemsAsData(t *testing.T) {
	p := BuildPrompt(items("a1", "b2"))
	for _, want := range []string{"id=a1", "Title a1", "Content b2", "DATA", "Indonesian"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
}

// Only ids that were in the batch may be written: the model reads memory text
// that came from untrusted conversations.
func TestParseKeepsOnlyBatchIDs(t *testing.T) {
	raw := `{"items":[{"id":"a1","aliases":["BCARDN","bca rdn"]},{"id":"zz","aliases":["x"]},{"id":"b2","aliases":[]}]}`
	got, err := Parse(raw, items("a1", "b2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || strings.Join(got["a1"], ",") != "bcardn,bca rdn" || got["b2"] == nil || len(got["b2"]) != 0 {
		t.Fatalf("want a1 cleaned and b2 empty-but-present, nothing for zz: %#v", got)
	}
}

type fakeStore struct {
	pending []Item
	applied map[string][]string
}

func (f *fakeStore) fetch(_ context.Context, limit int) ([]Item, error) {
	n := min(limit, len(f.pending))
	return append([]Item(nil), f.pending[:n]...), nil
}

func (f *fakeStore) apply(_ context.Context, id string, a []string) error {
	f.applied[id] = a
	for i, it := range f.pending {
		if it.ID == id {
			f.pending = append(f.pending[:i], f.pending[i+1:]...)
			break
		}
	}
	return nil
}

// A step aliases one batch. Items the model skipped get an empty list, so
// a resumed run does not ask about them forever.
func TestStepAppliesOneBatch(t *testing.T) {
	fs := &fakeStore{pending: items("a1", "b2", "c3"), applied: map[string][]string{}}
	b := Backfiller{Batch: 2, Fetch: fs.fetch, Apply: fs.apply,
		Run: func(_ context.Context, prompt, schema, sys string) (string, error) {
			return `{"items":[{"id":"a1","aliases":["bcardn"]}]}`, nil
		}}
	n, err := b.Step(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("want 2 items done, got %d %v", n, err)
	}
	if strings.Join(fs.applied["a1"], ",") != "bcardn" || fs.applied["b2"] == nil || len(fs.applied["b2"]) != 0 {
		t.Fatalf("want a1 aliased and b2 marked done with none: %#v", fs.applied)
	}
	if len(fs.pending) != 1 {
		t.Fatalf("one item should remain, got %d", len(fs.pending))
	}
}

// A failed spawn applies nothing: the batch stays pending for the next run.
func TestStepSpawnFailureAppliesNothing(t *testing.T) {
	fs := &fakeStore{pending: items("a1"), applied: map[string][]string{}}
	b := Backfiller{Batch: 2, Fetch: fs.fetch, Apply: fs.apply,
		Run: func(context.Context, string, string, string) (string, error) { return "", errors.New("killed") }}
	if _, err := b.Step(context.Background()); err == nil || len(fs.applied) != 0 {
		t.Fatalf("want an error and nothing applied, got %v %#v", err, fs.applied)
	}
}

func TestStepNothingLeft(t *testing.T) {
	fs := &fakeStore{applied: map[string][]string{}}
	b := Backfiller{Batch: 2, Fetch: fs.fetch, Apply: fs.apply,
		Run: func(context.Context, string, string, string) (string, error) {
			t.Fatal("no spawn when nothing is pending")
			return "", nil
		}}
	if n, err := b.Step(context.Background()); n != 0 || err != nil {
		t.Fatalf("want 0, nil got %d %v", n, err)
	}
}

// RunAll partitions the pending memories up front so parallel workers never
// alias the same memory twice, and reports every finished batch.
func TestRunAllPartitionsAcrossWorkers(t *testing.T) {
	all := items("a", "b", "c", "d", "e")
	var mu sync.Mutex
	applied := map[string]int{}
	b := Backfiller{Batch: 2,
		Apply: func(_ context.Context, id string, a []string) error {
			mu.Lock()
			applied[id]++
			mu.Unlock()
			return nil
		},
		Run: func(_ context.Context, prompt, schema, sys string) (string, error) { return `{"items":[]}`, nil },
	}
	var reported int
	done, failed := b.RunAll(context.Background(), all, 3, func(n int, err error) {
		mu.Lock()
		reported += n
		mu.Unlock()
	})
	if done != 5 || failed != 0 || reported != 5 {
		t.Fatalf("want 5 done in batches, got done %d failed %d reported %d", done, failed, reported)
	}
	for _, it := range all {
		if applied[it.ID] != 1 {
			t.Fatalf("%s applied %d times", it.ID, applied[it.ID])
		}
	}
}

func TestBuildPromptAsksForIndonesianWords(t *testing.T) {
	p := BuildPrompt(items("a1"))
	if !strings.Contains(p, "at least 2 Indonesian") || !strings.Contains(p, "single distinctive words") {
		t.Fatalf("the prompt must push for Indonesian terms and single words:\n%s", p)
	}
	// "must set" became the alias "wajib", which then matched every on-call
	// "wajib diisi" ticket: generic words are never aliases, in any language.
	for _, banned := range []string{`"wajib"`, `"gagal"`, `"harus"`} {
		if !strings.Contains(p, banned) {
			t.Fatalf("the prompt must name %s as a forbidden generic alias:\n%s", banned, p)
		}
	}
	if strings.Contains(p, `"gagal" for failed`) || strings.Contains(p, `"kendala" for problem`) {
		t.Fatal("generic words must not be offered as good alias examples")
	}
}

// RunAll stops dispatching batches once Stop says so (the quota cap); the
// rest stay pending for a later run.
func TestRunAllHonoursStop(t *testing.T) {
	all := items("a", "b", "c", "d", "e", "f")
	var mu sync.Mutex
	ran := 0
	b := Backfiller{Batch: 1,
		Apply: func(context.Context, string, []string) error { return nil },
		Run: func(context.Context, string, string, string) (string, error) {
			mu.Lock()
			ran++
			mu.Unlock()
			return `{"items":[]}`, nil
		},
		Stop: func() bool { mu.Lock(); defer mu.Unlock(); return ran >= 2 },
	}
	done, _ := b.RunAll(context.Background(), all, 1, nil)
	if done != 2 {
		t.Fatalf("want 2 batches before the stop, got %d", done)
	}
}
