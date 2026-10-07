package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/graph"
)

type fakeStore struct {
	mu      sync.Mutex
	cursor  map[string]int64
	batches int
}

func (f *fakeStore) GetCursor(_ context.Context, sid string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursor[sid], nil
}

func (f *fakeStore) SetCursor(_ context.Context, sid, _ string, line, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursor[sid] = line
	return nil
}

func (f *fakeStore) EntityNames(context.Context, string, int) ([]string, error) { return nil, nil }

func (f *fakeStore) EntityNamesGlobal(context.Context, int) ([]string, error) { return nil, nil }

func (f *fakeStore) SaveBatch(_ context.Context, _, _ string, _ int64, mems []graph.MemoryIn) ([]graph.SaveOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches++
	out := make([]graph.SaveOutcome, len(mems))
	for i, m := range mems {
		out[i] = graph.SaveOutcome{Title: m.Title, Kind: m.Kind, New: true}
	}
	return out, nil
}

func (f *fakeStore) ApplyFeedback(context.Context, int64, []graph.Verdict) error { return nil }

func (f *fakeStore) at(sid string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursor[sid]
}

type spawnLog struct {
	mu      sync.Mutex
	prompts []string
	err     error
}

func (s *spawnLog) run(_ context.Context, prompt string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, prompt)
	if s.err != nil {
		return "", s.err
	}
	return `{"memories":[{"title":"Port is 7690","content":"The daemon listens on 7690.","type":"fact"}]}`, nil
}

func (s *spawnLog) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts)
}

func conversation(t *testing.T, texts ...string) string {
	t.Helper()
	var b strings.Builder
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		line, _ := json.Marshal(map[string]any{"type": role, "message": map[string]any{"role": role, "content": text}})
		b.Write(line)
		b.WriteByte('\n')
	}
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func testWorker(maxChars int) (*Worker, *fakeStore, *spawnLog) {
	cfg := config.Default()
	cfg.MaxTranscriptChars = maxChars
	st := &fakeStore{cursor: map[string]int64{}}
	sp := &spawnLog{}
	w := &Worker{
		Store: st, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Spawn: sp.run, fails: map[string]int{}, lastRun: map[string]int64{}, cooldown: map[string]int64{},
		shrink: map[string]int{},
	}
	return w, st, sp
}

func TestWorkerDefersShortDeltaWithoutMovingTheCursor(t *testing.T) {
	w, st, sp := testWorker(24000)
	p := conversation(t, "ok", "Done.")
	if _, err := w.process(Job{SessionID: "s", TranscriptPath: p, CWD: "/c"}); err != nil {
		t.Fatal(err)
	}
	if sp.count() != 0 || st.at("s") != 0 {
		t.Fatalf("a short turn must wait for more context: spawns=%d cursor=%d", sp.count(), st.at("s"))
	}
}

func TestWorkerFinalJobExtractsShortDelta(t *testing.T) {
	w, st, sp := testWorker(24000)
	p := conversation(t, "push it", "Committed as bddae456.")
	if _, err := w.process(Job{SessionID: "s", TranscriptPath: p, CWD: "/c", Final: true}); err != nil {
		t.Fatal(err)
	}
	if sp.count() != 1 || st.at("s") != 2 {
		t.Fatalf("a final job must save the closing exchange: spawns=%d cursor=%d", sp.count(), st.at("s"))
	}
}

func TestWorkerChunksBigDeltasAndRequeuesTheRest(t *testing.T) {
	w, st, sp := testWorker(500)
	var requeued []Job
	w.Requeue = func(j Job) { requeued = append(requeued, j) }
	long := strings.Repeat("decision context ", 20)
	p := conversation(t, long, long, long, long)
	if _, err := w.process(Job{SessionID: "s", TranscriptPath: p, CWD: "/c"}); err != nil {
		t.Fatal(err)
	}
	if sp.count() != 1 || st.at("s") != 1 || len(requeued) != 1 {
		t.Fatalf("want one chunk extracted, cursor after it, rest requeued: spawns=%d cursor=%d requeued=%d",
			sp.count(), st.at("s"), len(requeued))
	}
}

func TestWorkerFailuresNeverMoveTheCursor(t *testing.T) {
	w, st, sp := testWorker(24000)
	sp.err = errors.New("claude spawn: signal: killed")
	long := strings.Repeat("worth saving ", 30)
	p := conversation(t, long, long, long, long)
	for i := 0; i < 6; i++ {
		_, _ = w.process(Job{SessionID: "s", TranscriptPath: p, CWD: "/c"})
	}
	if st.at("s") != 0 {
		t.Fatalf("failed extractions must keep the delta, cursor=%d", st.at("s"))
	}
	if sp.count() > 4 {
		t.Fatalf("repeated failures must back off, spawned %d times", sp.count())
	}
	if len(sp.prompts) < 2 || len(sp.prompts[1]) >= len(sp.prompts[0]) {
		t.Fatal("a retry after a failure must try a smaller chunk")
	}
}

func TestWorkerDefersWhenUsageIsOverTheCap(t *testing.T) {
	w, st, sp := testWorker(24000)
	w.Cfg.ExtractMaxUsage = 0.85
	w.Usage = func(context.Context) (float64, error) { return 0.9, nil }
	long := strings.Repeat("worth saving ", 30)
	p := conversation(t, long, long)
	rep, err := w.process(Job{SessionID: "s", TranscriptPath: p, CWD: "/c"})
	if err != nil {
		t.Fatal(err)
	}
	if sp.count() != 0 || st.at("s") != 0 || !strings.Contains(rep.Skipped, "usage") {
		t.Fatalf("over the usage cap: no spawn, cursor kept, reason shown; spawns=%d cursor=%d skipped=%q",
			sp.count(), st.at("s"), rep.Skipped)
	}
}
