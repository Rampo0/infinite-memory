package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/extract"
	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/project"
)

type auditRow struct {
	src    graph.SessionSource
	unread int64
	idle   time.Duration
}

func cmdAuditSaves(args []string) {
	cfg := config.Load()
	store, err := graph.New(cfg.MemgraphURI, cfg.MemgraphUser, cfg.MemgraphPass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "driver:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	since := time.Now().Add(-7 * 24 * time.Hour)
	if len(args) > 0 && args[0] == "--adopt" {
		n, err := adoptRecent(ctx, store, cfg, since)
		if err != nil {
			fmt.Fprintln(os.Stderr, "adopt:", err)
			os.Exit(1)
		}
		fmt.Printf("adopted %d earlier sessions for the sweep\n", n)
	}
	cands, err := store.SweepCandidates(ctx, since.Unix())
	if err != nil {
		fmt.Fprintln(os.Stderr, "memgraph:", err)
		os.Exit(1)
	}
	behind := auditBehind(cands)
	fmt.Printf("%d sessions seen in the last 7 days: %d fully saved, %d with unread transcript lines\n",
		len(cands), len(cands)-len(behind), len(behind))
	for _, r := range behind {
		fmt.Printf("  %s  %4d lines unread, idle %s  %s\n", short(r.src.ID), r.unread, r.idle.Round(time.Minute),
			filepath.Base(r.src.CWD))
	}
	if len(behind) > 0 {
		fmt.Println("idle sessions are swept every 10 minutes; one still listed after that is worth a look in imemd.log")
	}
}

func auditBehind(cands []graph.SessionSource) []auditRow {
	var out []auditRow
	for _, c := range cands {
		fi, err := os.Stat(c.TranscriptPath)
		if err != nil {
			continue
		}
		lines, err := extract.CountLines(c.TranscriptPath)
		if err == nil && lines > c.Cursor {
			out = append(out, auditRow{src: c, unread: lines - c.Cursor, idle: time.Since(fi.ModTime())})
		}
	}
	return out
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func adoptRecent(ctx context.Context, store *graph.Store, cfg config.Config, since time.Time) (int, error) {
	files, _ := filepath.Glob(filepath.Join(config.ClaudeProjectsDir(), "*", "*.jsonl"))
	n := 0
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil || fi.ModTime().Before(since) {
			continue
		}
		sid := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		cur, err := store.GetCursor(ctx, sid)
		cwd, headless := extract.TranscriptMeta(f)
		if err != nil || cur == 0 || cwd == "" || cfg.Ignored(cwd) {
			continue
		}
		src := graph.SessionSource{ID: sid, ProjectKey: project.ResolveKey(cwd), TranscriptPath: f, CWD: cwd, Agent: headless}
		if err := store.TouchSession(ctx, src, time.Now().Unix()); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
