package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Rampo0/infinite-memory/internal/expand"
)

func TestExpandedLine(t *testing.T) {
	cases := []struct {
		name string
		res  expand.Result
		want string
	}{
		{"no expansion", expand.Result{}, ""},
		// Intent-only is not an expansion: nothing was added to the search.
		{"intent but no terms", expand.Result{Intent: "something"}, ""},
		{"terms without intent", expand.Result{Added: []string{"a", "b"}}, "+2 expanded"},
		{"terms with intent",
			expand.Result{Added: []string{"a"}, Intent: "debug the guard"},
			"+1 expanded\n  \u21b3 debug the guard"},
		// A timeout and an empty answer both add nothing; only the message
		// tells the user which one happened.
		{"timeout", expand.Result{MS: 13014, Err: fmt.Errorf("%w: killed", context.DeadlineExceeded)},
			"expansion timed out (13014ms)"},
		{"other failure", expand.Result{Err: errors.New("boom")}, "expansion failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expandedLine(c.res); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestVocabCacheTTL(t *testing.T) {
	c := newVocabCache()
	now := int64(0)
	c.now = func() time.Time { return time.Unix(now, 0) }
	c.put("/p", expand.Vocab{Entities: []string{"e"}})

	if v, ok := c.get("/p"); !ok || len(v.Entities) != 1 {
		t.Fatal("fresh entry should hit")
	}
	if _, ok := c.get("/other"); ok {
		t.Fatal("unknown project must miss")
	}
	now += int64(vocabTTL.Seconds()) + 1
	if _, ok := c.get("/p"); ok {
		t.Fatal("entry past TTL must miss")
	}
}

func TestVocabCacheIsPerProject(t *testing.T) {
	c := newVocabCache()
	c.put("/a", expand.Vocab{Entities: []string{"alpha"}})
	c.put("/b", expand.Vocab{Entities: []string{"beta"}})
	va, _ := c.get("/a")
	vb, _ := c.get("/b")
	if strings.Join(va.Entities, "") == strings.Join(vb.Entities, "") {
		t.Fatal("projects must not share a vocabulary entry")
	}
}
