package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Rampo0/infinite-memory/internal/extract"
)

func TestCachedUsageProbesOncePerWindow(t *testing.T) {
	calls := 0
	probe := func(context.Context) (extract.Quota, error) {
		calls++
		return extract.Quota{FiveHour: 0.4}, nil
	}
	now := time.Unix(1000, 0)
	u := cachedUsage(probe, 10*time.Minute, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if v, err := u(context.Background()); err != nil || v != 0.4 {
			t.Fatalf("got %v, %v", v, err)
		}
	}
	now = now.Add(11 * time.Minute)
	_, _ = u(context.Background())
	if calls != 2 {
		t.Fatalf("want one probe per window, got %d", calls)
	}
}
