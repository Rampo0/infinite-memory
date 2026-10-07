package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/Rampo0/infinite-memory/internal/extract"
)

func cachedUsage(probe func(context.Context) (extract.Quota, error), ttl time.Duration, now func() time.Time) func(context.Context) (float64, error) {
	var mu sync.Mutex
	var at time.Time
	var val float64
	var last error
	return func(ctx context.Context) (float64, error) {
		mu.Lock()
		defer mu.Unlock()
		if !at.IsZero() && now().Sub(at) < ttl {
			return val, last
		}
		q, err := probe(ctx)
		at, val, last = now(), q.FiveHour, err
		return val, last
	}
}

func usageProbe(r *extract.Runner, model string) func(context.Context) (extract.Quota, error) {
	return func(ctx context.Context) (extract.Quota, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return r.Quota(ctx, model)
	}
}
