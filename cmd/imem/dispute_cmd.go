package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/graph"
)

func cmdDispute(args []string) {
	if len(args) < 2 || (args[0] != "wrong" && args[0] != "outdated") {
		fmt.Fprintln(os.Stderr, "usage: imem dispute wrong|outdated [--archive] <memory id or title words>")
		os.Exit(2)
	}
	verdict, rest, archive := args[0], args[1:], false
	if rest[0] == "--archive" {
		archive, rest = true, rest[1:]
	}
	q := strings.TrimSpace(strings.Join(rest, " "))
	cfg := config.Load()
	store, err := graph.New(cfg.MemgraphURI, cfg.MemgraphUser, cfg.MemgraphPass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "driver:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	targets, err := store.MemoryTargets(ctx, q)
	target, ok := pickPinTarget(targets, q)
	if err != nil || !ok {
		reportPinChoices(targets, q)
		os.Exit(1)
	}
	if err := disputeMemory(ctx, store, target.ID, verdict, archive); err != nil {
		fmt.Fprintln(os.Stderr, "memgraph:", err)
		os.Exit(1)
	}
	suffix := ""
	if archive {
		suffix = " (archived)"
	}
	fmt.Printf("%s: [%s] %s%s\n", verdict, target.Kind, target.Title, suffix)
}

func disputeMemory(ctx context.Context, store *graph.Store, id, verdict string, archive bool) error {
	if err := store.ApplyFeedback(ctx, time.Now().Unix(), []graph.Verdict{{ID: id, Verdict: verdict}}); err != nil {
		return err
	}
	if !archive {
		return nil
	}
	return store.Archive(ctx, []string{id})
}
