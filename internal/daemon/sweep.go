package daemon

import (
	"context"
	"os"
	"time"

	"github.com/Rampo0/infinite-memory/internal/extract"
	"github.com/Rampo0/infinite-memory/internal/graph"
)

const (
	sweepEvery    = 10 * time.Minute
	sweepBootWait = time.Minute
	sweepIdle     = 3 * time.Minute
	sweepHorizon  = 7 * 24 * time.Hour
)

type sweepStore interface {
	SweepCandidates(ctx context.Context, since int64) ([]graph.SessionSource, error)
}

type sweeper struct {
	store   sweepStore
	notify  func(source string, j Job)
	valid   func(path string) bool
	ignored func(cwd string) bool
	now     func() time.Time
	idle    time.Duration
}

func (sw *sweeper) run(ctx context.Context) int {
	cands, err := sw.store.SweepCandidates(ctx, sw.now().Add(-sweepHorizon).Unix())
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range cands {
		if !sw.behind(c) {
			continue
		}
		sw.notify("sweep", Job{SessionID: c.ID, TranscriptPath: c.TranscriptPath, CWD: c.CWD, Agent: c.Agent, Final: true})
		n++
	}
	return n
}

func (sw *sweeper) behind(c graph.SessionSource) bool {
	if sw.ignored(c.CWD) || !sw.valid(c.TranscriptPath) {
		return false
	}
	fi, err := os.Stat(c.TranscriptPath)
	if err != nil || sw.now().Sub(fi.ModTime()) < sw.idle || fi.ModTime().Unix() <= c.UpdatedAt {
		return false
	}
	lines, err := extract.CountLines(c.TranscriptPath)
	return err == nil && lines > c.Cursor
}

func (s *server) startSweepLoop() {
	sw := &sweeper{
		store:   s.store,
		notify:  s.queue.Notify,
		valid:   func(p string) bool { return validTranscriptPath(p, s.agentRoots()) },
		ignored: s.cfg.Ignored,
		now:     time.Now,
		idle:    sweepIdle,
	}
	go func() {
		time.Sleep(sweepBootWait)
		for {
			if n := sw.run(context.Background()); n > 0 {
				s.log.Info("sweep queued sessions with unsaved turns", "sessions", n)
			}
			time.Sleep(sweepEvery)
		}
	}()
}
