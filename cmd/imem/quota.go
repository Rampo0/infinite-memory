package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/extract"
)

// exitQuota is the exit code of a bulk job stopped by its quota cap: the work
// left is pending, run it again after the window resets.
const exitQuota = 3

// quotaGate stops bulk jobs before they eat the quota the user and the
// ai-review / on-call agents (which pause at 60% of the 5-hour window) need.
// It probes at most once a minute, latches once usage reaches max, and fails
// safe: an unreadable quota stops the job. max <= 0 disables it.
type quotaGate struct {
	probe   func() (extract.Quota, error)
	max     float64
	now     func() time.Time
	mu      sync.Mutex
	last    time.Time
	halted  bool
	reading extract.Quota
}

func newQuotaGate(probe func() (extract.Quota, error), max float64, now func() time.Time) *quotaGate {
	return &quotaGate{probe: probe, max: max, now: now}
}

func (g *quotaGate) stop() bool {
	if g.max <= 0 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.halted {
		return true
	}
	if !g.last.IsZero() && g.now().Sub(g.last) < time.Minute {
		return false
	}
	g.last = g.now()
	q, err := g.probe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "quota unreadable, stopping to be safe: %v\n", err)
		g.halted = true
		return true
	}
	g.reading = q
	if q.FiveHour >= g.max {
		fmt.Fprintf(os.Stderr, "stopping: 5-hour usage %.0f%% >= cap %.0f%% (resets %s); re-run to resume\n",
			q.FiveHour*100, g.max*100, time.Unix(q.FiveHourResets, 0).Format("15:04"))
		g.halted = true
	}
	return g.halted
}

func (g *quotaGate) stopped() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.halted
}

// probeQuota is the live probe: one tiny isolated expand_model call.
func probeQuota(cfg config.Config) func() (extract.Quota, error) {
	runner := extract.NewRunner(cfg)
	return func() (extract.Quota, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		return runner.Quota(ctx, cfg.ExpandModel)
	}
}

// cmdQuota prints the subscription usage; --resets prints only the 5-hour
// window's reset time (unix seconds), for scripts.
func cmdQuota(args []string) {
	q, err := probeQuota(config.Load())()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(args) > 0 && args[0] == "--resets" {
		fmt.Println(q.FiveHourResets)
		return
	}
	fmt.Printf("5-hour window %.0f%% (resets %s), 7-day window %.0f%%\n",
		q.FiveHour*100, time.Unix(q.FiveHourResets, 0).Format("2006-01-02 15:04"), q.SevenDay*100)
}
