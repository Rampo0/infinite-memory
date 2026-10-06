package daemon

import (
	"context"
	"fmt"
	"github.com/Rampo0/infinite-memory/internal/consolidate"
	"github.com/Rampo0/infinite-memory/internal/extract"
	"time"

	"github.com/Rampo0/infinite-memory/internal/backup"
)

// backupCheckCap bounds how often the loop wakes. The loop compares wall-clock
// times rather than counting ticks, so a laptop that slept through several
// intervals still backs up on the first tick after waking.
const backupCheckCap = 10 * time.Minute

// backupBootDelay lets Memgraph finish starting after a Docker restart before
// the first check runs.
const backupBootDelay = 30 * time.Second

// startBackupLoop runs periodic dumps in the background. It is the only
// long-lived timer in the daemon besides the extract queue.
func (s *server) startBackupLoop() {
	dir := s.cfg.BackupPath()
	if err := backup.CleanTemp(dir); err != nil {
		s.log.Warn("backup temp cleanup failed", "dir", dir, "err", err)
	}
	every := s.cfg.BackupInterval() / 8
	if every > backupCheckCap {
		every = backupCheckCap
	}
	if every < time.Second {
		every = time.Second
	}
	s.log.Info("backup loop started", "dir", dir, "interval", s.cfg.BackupInterval(), "keep", s.cfg.BackupKeep)
	go func() {
		time.Sleep(backupBootDelay)
		s.maybeBackup()
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			s.maybeBackup()
		}
	}()
}

// maybeBackup dumps only when the newest backup on disk is older than the
// configured interval, which makes the schedule resumable across sleep and
// daemon restarts without any persisted timer state.
func (s *server) maybeBackup() {
	dir := s.cfg.BackupPath()
	latest, ok, err := backup.Latest(dir)
	if err != nil {
		s.log.Warn("backup listing failed", "dir", dir, "err", err)
		return
	}
	if ok && time.Since(latest.ModTime) < s.cfg.BackupInterval() {
		return
	}
	if _, err := s.runBackup(context.Background()); err != nil {
		s.log.Warn("scheduled backup failed", "err", err)
	}
}

type backupResult struct {
	Info       backup.Info
	Statements int
}

// runBackup dumps the graph and rotates the directory down to BackupKeep. The
// mutex keeps a scheduled run and a manual POST /v1/backup from overlapping.
func (s *server) runBackup(ctx context.Context) (backupResult, error) {
	s.backupMu.Lock()
	defer s.backupMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	stmts, err := s.store.Dump(ctx)
	if err != nil {
		s.setBackupState(nil, err.Error())
		return backupResult{}, err
	}
	// A dump with no nodes means Memgraph came back blank (fresh volume, wiped
	// DB) — note it is not zero-length, since EnsureSchema recreates the
	// indexes and constraints at boot. Writing it would let two good backups
	// age out and be replaced by two useless ones, so keep what is on disk.
	if !backup.HasData(stmts) {
		s.log.Warn("backup skipped: dump has no nodes", "dir", s.cfg.BackupPath(), "statements", len(stmts))
		s.setBackupState(nil, fmt.Sprintf("dump had no nodes (%d schema-only statements)", len(stmts)))
		return backupResult{}, nil
	}

	dir := s.cfg.BackupPath()
	info, err := backup.Write(dir, stmts, time.Now())
	if err != nil {
		s.setBackupState(nil, err.Error())
		return backupResult{}, err
	}
	// Write first, prune second: the new file is durable before any old one
	// is removed, so a failure here still leaves a complete set behind.
	if err := backup.Prune(dir, s.cfg.BackupKeep); err != nil {
		s.log.Warn("backup prune failed", "dir", dir, "err", err)
	}

	s.setBackupState(&info, "")
	s.log.Info("backup written", "path", info.Path, "bytes", info.Size, "statements", len(stmts))
	return backupResult{Info: info, Statements: len(stmts)}, nil
}

// setBackupState uses its own mutex so reading status never waits on a dump
// that is holding backupMu for up to a minute.
func (s *server) setBackupState(info *backup.Info, errMsg string) {
	s.backupStateMu.Lock()
	defer s.backupStateMu.Unlock()
	if info != nil {
		s.lastBackup = *info
	}
	s.lastBackupErr = errMsg
}

func (s *server) backupStatus() (backup.Info, string) {
	s.backupStateMu.Lock()
	defer s.backupStateMu.Unlock()
	return s.lastBackup, s.lastBackupErr
}

// startConsolidateLoop merges near-duplicate memories every
// consolidate_interval_hours (opt-in: consolidate_enabled). The first run
// waits a full interval, so a restart never triggers a burst of spawns.
func (s *server) startConsolidateLoop(runner *extract.Runner) {
	every := time.Duration(s.cfg.ConsolidateIntervalHours) * time.Hour
	s.log.Info("consolidate loop started", "interval", every, "min_jaccard", s.cfg.ConsolidateMinJaccard)
	c := consolidate.Consolidator{
		Run: func(ctx context.Context, prompt, schema, sys string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
			defer cancel()
			return runner.RunSchema(ctx, extract.Request{Prompt: prompt, Schema: schema, SystemPrompt: sys, Isolated: true, Effort: "low"})
		},
		Apply: consolidate.StoreApply(s.store),
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			ctx := context.Background()
			live, err := s.store.LiveMemories(ctx)
			if err != nil {
				s.log.Warn("consolidate: list failed", "err", err)
				continue
			}
			merged := 0
			for _, cl := range consolidate.Clusters(consolidate.FromLive(live), s.cfg.ConsolidateMinJaccard, 30, 8) {
				ms, err := c.Process(ctx, cl)
				if err != nil {
					s.log.Warn("consolidate: cluster failed", "err", err)
				}
				merged += len(ms)
			}
			s.log.Info("consolidated", "merges", merged)
		}
	}()
}
