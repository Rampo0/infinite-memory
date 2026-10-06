package main

import (
	"errors"
	"testing"
	"time"

	"github.com/Rampo0/infinite-memory/internal/extract"
)

// The bulk jobs (alias backfill, consolidation) share the subscription with
// the ai-review / on-call agents, which pause at 60% of the 5-hour window.
// The gate probes at most once a minute and latches once usage hits the cap.
func TestQuotaGate(t *testing.T) {
	now := time.Unix(1000, 0)
	usage, probes := 0.30, 0
	g := newQuotaGate(func() (extract.Quota, error) { probes++; return extract.Quota{FiveHour: usage}, nil },
		0.5, func() time.Time { return now })
	if g.stop() || probes != 1 {
		t.Fatalf("30%% < 50%%: keep going (probes %d)", probes)
	}
	usage = 0.55
	now = now.Add(30 * time.Second)
	if g.stop() || probes != 1 {
		t.Fatal("within a minute the last reading stands, no new probe")
	}
	now = now.Add(31 * time.Second)
	if !g.stop() || probes != 2 || !g.stopped() {
		t.Fatal("past a minute it re-probes and stops at 55%")
	}
	usage = 0.10
	now = now.Add(5 * time.Minute)
	if !g.stop() {
		t.Fatal("once stopped it stays stopped for this run")
	}
}

func TestQuotaGateFailsSafe(t *testing.T) {
	g := newQuotaGate(func() (extract.Quota, error) { return extract.Quota{}, errors.New("probe failed") },
		0.5, time.Now)
	if !g.stop() {
		t.Fatal("an unreadable quota must stop the job, not let it run blind")
	}
}

func TestQuotaGateDisabled(t *testing.T) {
	g := newQuotaGate(func() (extract.Quota, error) { t.Fatal("no probe when disabled"); return extract.Quota{}, nil }, 0, time.Now)
	if g.stop() {
		t.Fatal("max 0 disables the gate")
	}
}
