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
