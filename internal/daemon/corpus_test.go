package daemon

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCorpusCacheTTL(t *testing.T) {
	calls, n := 0, 100
	now := time.Unix(1000, 0)
	c := &corpusCache{ttl: time.Minute, now: func() time.Time { return now },
		count: func(context.Context) (int, error) { calls++; return n, nil }}

	if got := c.Get(context.Background()); got != 100 || calls != 1 {
		t.Fatalf("first read: got %d after %d calls", got, calls)
	}
	n = 200
	now = now.Add(30 * time.Second)
	if got := c.Get(context.Background()); got != 100 || calls != 1 {
		t.Fatalf("within the TTL the cached count is served: got %d after %d calls", got, calls)
	}
	now = now.Add(31 * time.Second)
	if got := c.Get(context.Background()); got != 200 || calls != 2 {
		t.Fatalf("past the TTL it recounts: got %d after %d calls", got, calls)
	}
}

// A failed count keeps the last good value: a Memgraph blip must not switch
// idf off mid-session and reshuffle every ranking.
func TestCorpusCacheKeepsLastGoodOnError(t *testing.T) {
	now := time.Unix(1000, 0)
	fail := false
	c := &corpusCache{ttl: time.Minute, now: func() time.Time { return now },
		count: func(context.Context) (int, error) {
			if fail {
				return 0, errors.New("memgraph down")
			}
			return 42, nil
		}}
	c.Get(context.Background())
	fail, now = true, now.Add(2*time.Minute)
	if got := c.Get(context.Background()); got != 42 {
		t.Fatalf("want the last good count, got %d", got)
	}
}
