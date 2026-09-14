package daemon

import (
	"log/slog"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu    sync.Mutex
	calls []Job
	done  chan struct{}
}

func (r *recorder) process(j Job) error {
	r.mu.Lock()
	r.calls = append(r.calls, j)
	r.mu.Unlock()
	select {
	case r.done <- struct{}{}:
	default:
	}
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func newTestQueue(debounce time.Duration, rec *recorder) *Queue {
	q := NewQueue(debounce, rec.process, slog.Default())
	q.Start()
	return q
}

func TestQueueDebounceCoalesces(t *testing.T) {
	rec := &recorder{done: make(chan struct{}, 8)}
	q := newTestQueue(50*time.Millisecond, rec)
	defer q.Stop()

	for i := 0; i < 3; i++ {
		q.Notify("stop", Job{SessionID: "s1", TranscriptPath: "/t", CWD: "/c"})
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-rec.done:
	case <-time.After(2 * time.Second):
		t.Fatal("job never ran")
	}
	time.Sleep(100 * time.Millisecond)
	if n := rec.count(); n != 1 {
		t.Fatalf("3 rapid stops should coalesce into 1 job, got %d", n)
	}
}

func TestQueueSessionEndImmediate(t *testing.T) {
	rec := &recorder{done: make(chan struct{}, 8)}
	q := newTestQueue(10*time.Second, rec) // long debounce: only session_end can fire
	defer q.Stop()

	q.Notify("stop", Job{SessionID: "s2", TranscriptPath: "/t", CWD: "/c"})
	q.Notify("session_end", Job{SessionID: "s2", TranscriptPath: "/t2", CWD: "/c"})
	select {
	case <-rec.done:
	case <-time.After(2 * time.Second):
		t.Fatal("session_end should enqueue immediately")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 || rec.calls[0].TranscriptPath != "/t2" {
		t.Fatalf("want 1 call with latest job data, got %+v", rec.calls)
	}
}

func TestQueueFlushUsesPendingData(t *testing.T) {
	rec := &recorder{done: make(chan struct{}, 8)}
	q := newTestQueue(10*time.Second, rec)
	defer q.Stop()

	q.Notify("stop", Job{SessionID: "s3", TranscriptPath: "/pending", CWD: "/c"})
	if err := q.Flush(Job{SessionID: "s3"}); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 || rec.calls[0].TranscriptPath != "/pending" {
		t.Fatalf("flush should reuse pending job data, got %+v", rec.calls)
	}
}

func TestQueueFlushUnknownSession(t *testing.T) {
	rec := &recorder{done: make(chan struct{}, 8)}
	q := newTestQueue(time.Second, rec)
	defer q.Stop()
	if err := q.Flush(Job{SessionID: "nope"}); err == nil {
		t.Fatal("flush without transcript path should error")
	}
}

func TestFlushBudgetWithinBudget(t *testing.T) {
	rec := &recorder{done: make(chan struct{}, 8)}
	q := newTestQueue(time.Hour, rec)
	defer q.Stop()

	ok, err := q.FlushBudget(Job{SessionID: "s1", TranscriptPath: "/t"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("fast job should finish inside the budget")
	}
	if rec.count() != 1 {
		t.Fatalf("want 1 process call, got %d", rec.count())
	}
}

func TestFlushBudgetTimesOut(t *testing.T) {
	released := make(chan struct{})
	finished := make(chan struct{})
	q := NewQueue(time.Hour, func(Job) error {
		<-released
		close(finished)
		return nil
	}, slog.Default())
	q.Start()
	defer q.Stop()

	ok, err := q.FlushBudget(Job{SessionID: "s1", TranscriptPath: "/t"}, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("slow job should report not-done")
	}
	// The work must still be running, not cancelled, and must complete.
	close(released)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("work abandoned after budget expiry; it must finish in the background")
	}
}

func TestQueueOnPendingFires(t *testing.T) {
	rec := &recorder{done: make(chan struct{}, 8)}
	q := NewQueue(time.Hour, rec.process, slog.Default())
	var mu sync.Mutex
	var got []string
	q.OnPending = func(sid string, dueAt int64) {
		mu.Lock()
		defer mu.Unlock()
		if dueAt <= 0 {
			t.Errorf("dueAt must be a real timestamp, got %d", dueAt)
		}
		got = append(got, sid)
	}
	q.Start()
	defer q.Stop()

	q.Notify("stop", Job{SessionID: "s1", TranscriptPath: "/t"})
	q.Notify("session_end", Job{SessionID: "s2", TranscriptPath: "/t"})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Fatalf("want both branches to fire OnPending, got %v", got)
	}
}

func TestQueueNilOnPendingSafe(t *testing.T) {
	rec := &recorder{done: make(chan struct{}, 8)}
	q := newTestQueue(time.Hour, rec)
	defer q.Stop()
	q.Notify("stop", Job{SessionID: "s1", TranscriptPath: "/t"})
}
