package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rampo0/infinite-memory/internal/graph"
)

type sweepFake struct{ cands []graph.SessionSource }

func (f sweepFake) SweepCandidates(context.Context, int64) ([]graph.SessionSource, error) {
	return f.cands, nil
}

func transcriptLines(t *testing.T, n int, modified time.Time) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	data := ""
	for i := 0; i < n; i++ {
		data += `{"type":"user","message":{"role":"user","content":"x"}}` + "\n"
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, modified, modified); err != nil {
		t.Fatal(err)
	}
	return p
}

func testSweeper(cands []graph.SessionSource, now time.Time) (*sweeper, *[]Job) {
	var got []Job
	sw := &sweeper{
		store:   sweepFake{cands: cands},
		notify:  func(_ string, j Job) { got = append(got, j) },
		valid:   func(string) bool { return true },
		ignored: func(string) bool { return false },
		now:     func() time.Time { return now },
		idle:    3 * time.Minute,
	}
	return sw, &got
}

func TestSweepPicksIdleSessionsWithUnreadLines(t *testing.T) {
	now := time.Now()
	idle := now.Add(-10 * time.Minute)
	behind := transcriptLines(t, 5, idle)
	done := transcriptLines(t, 5, idle)
	active := transcriptLines(t, 5, now)
	sw, got := testSweeper([]graph.SessionSource{
		{ID: "behind", TranscriptPath: behind, CWD: "/c", Cursor: 3, UpdatedAt: idle.Add(-time.Hour).Unix()},
		{ID: "done", TranscriptPath: done, CWD: "/c", Cursor: 5, UpdatedAt: idle.Add(-time.Hour).Unix()},
		{ID: "active", TranscriptPath: active, CWD: "/c", Cursor: 1, UpdatedAt: idle.Add(-time.Hour).Unix()},
	}, now)
	sw.run(context.Background())
	if len(*got) != 1 || (*got)[0].SessionID != "behind" {
		t.Fatalf("want only the idle session with unread lines: %+v", *got)
	}
}

func TestSweepSkipsSessionsSavedAfterTheirLastWrite(t *testing.T) {
	now := time.Now()
	idle := now.Add(-10 * time.Minute)
	p := transcriptLines(t, 5, idle)
	sw, got := testSweeper([]graph.SessionSource{
		{ID: "s", TranscriptPath: p, CWD: "/c", Cursor: 2, UpdatedAt: now.Unix()},
	}, now)
	sw.run(context.Background())
	if len(*got) != 0 {
		t.Fatalf("a session saved after its last write has nothing new: %+v", *got)
	}
}

func TestSweepCarriesTheAgentFlag(t *testing.T) {
	now := time.Now()
	idle := now.Add(-10 * time.Minute)
	p := transcriptLines(t, 3, idle)
	sw, got := testSweeper([]graph.SessionSource{
		{ID: "bot", TranscriptPath: p, CWD: "/c", Agent: true, Cursor: 0},
	}, now)
	sw.run(context.Background())
	if len(*got) != 1 || !(*got)[0].Agent {
		t.Fatalf("a bot transcript must stay flagged: %+v", *got)
	}
}

func TestSweepTreatsOnlyLongIdleSessionsAsFinal(t *testing.T) {
	now := time.Now()
	recent := transcriptLines(t, 5, now.Add(-10*time.Minute))
	old := transcriptLines(t, 5, now.Add(-2*time.Hour))
	sw, got := testSweeper([]graph.SessionSource{
		{ID: "recent", TranscriptPath: recent, CWD: "/c", Cursor: 3},
		{ID: "old", TranscriptPath: old, CWD: "/c", Cursor: 3},
	}, now)
	sw.run(context.Background())
	final := map[string]bool{}
	for _, j := range *got {
		final[j.SessionID] = j.Final
	}
	if len(*got) != 2 || final["recent"] || !final["old"] {
		t.Fatalf("a session paused for minutes may go on; only a long-idle one gets its short tail saved: %+v", *got)
	}
}
