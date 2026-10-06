package daemon

import (
	"context"
	"sync"
	"time"
)

// corpusCache memoizes the live memory count, the N of the retriever's idf
// weights. It moves by a handful per extraction, so a minute-old value is as
// good as a fresh one and saves a count query per prompt.
type corpusCache struct {
	mu    sync.Mutex
	n     int
	at    time.Time
	ttl   time.Duration
	now   func() time.Time
	count func(context.Context) (int, error)
}

func newCorpusCache(count func(context.Context) (int, error)) *corpusCache {
	return &corpusCache{ttl: time.Minute, now: time.Now, count: count}
}

// Get returns the cached count, recounting past the TTL. A failed recount
// keeps the last good value (0 before any success, which disables idf).
func (c *corpusCache) Get(ctx context.Context) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && c.now().Sub(c.at) < c.ttl {
		return c.n
	}
	if n, err := c.count(ctx); err == nil {
		c.n, c.at = n, c.now()
	}
	return c.n
}
