package daemon

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func testLog() *saveLog {
	l := newSaveLog()
	l.now = func() int64 { return 1000 }
	return l
}

func rep(sid string, titles ...string) SaveReport {
	r := SaveReport{SessionID: sid, At: 1000, DurationMS: 1200}
	for _, t := range titles {
		r.Memories = append(r.Memories, SavedMemory{Title: t, Kind: "fact", New: true, Seen: 1})
	}
	if len(titles) == 0 {
		r.Skipped = "nothing worth saving"
	}
	return r
}

func TestSaveLogDrainOnce(t *testing.T) {
	l := testLog()
	l.Record(rep("s1", "alpha", "beta"))

	first := l.Drain("s1", -1)
	if first.Count != 2 || first.New != 2 || first.Batches != 1 {
		t.Fatalf("first drain: %+v", first)
	}
	if first.Summary == "" {
		t.Fatal("first drain should render a summary")
	}
	second := l.Drain("s1", -1)
	if second.Count != 0 || second.Batches != 0 || second.Summary != "" {
		t.Fatalf("second drain should be empty, got %+v", second)
	}
}

func TestSaveLogQuietNotRecorded(t *testing.T) {
	l := testLog()
	r := rep("s1", "alpha")
	r.quiet = true
	l.Record(r)
	if got := l.Drain("s1", -1); got.Count != 0 || got.Batches != 0 {
		t.Fatalf("quiet report must not surface, got %+v", got)
	}
}

func TestSaveLogRingBound(t *testing.T) {
	l := testLog()
	for i := 0; i < l.max+2; i++ {
		l.Record(rep("s1", fmt.Sprintf("m%d", i)))
	}
	ss := l.bySess["s1"]
	if len(ss.reports) != l.max {
		t.Fatalf("want %d reports kept, got %d", l.max, len(ss.reports))
	}
	if ss.dropped != 2 {
		t.Fatalf("want 2 dropped, got %d", ss.dropped)
	}
	if got := l.Drain("s1", -1); got.Batches != l.max {
		t.Fatalf("want %d batches drained, got %d", l.max, got.Batches)
	}
}

func TestSaveLogTTLEvicts(t *testing.T) {
	l := testLog()
	l.Record(rep("s1", "alpha"))
	l.now = func() int64 { return 1000 + int64(l.ttl/time.Second) + 1 }
	if got := l.Drain("s1", -1); got.Count != 0 {
		t.Fatalf("expired report must not surface, got %+v", got)
	}
}

func TestSaveLogSessionsIsolated(t *testing.T) {
	l := testLog()
	l.Record(rep("s1", "alpha"))
	l.Record(rep("s2", "beta", "gamma"))
	if got := l.Drain("s1", -1); got.Count != 1 {
		t.Fatalf("s1: want 1, got %d", got.Count)
	}
	if got := l.Drain("s2", -1); got.Count != 2 {
		t.Fatalf("s2 must be untouched by the s1 drain, got %d", got.Count)
	}
}

func TestSaveLogPeekDoesNotDrain(t *testing.T) {
	l := testLog()
	l.Record(rep("s1", "alpha"))
	if got, _ := l.Peek("s1"); len(got) != 1 {
		t.Fatalf("peek: want 1 report, got %d", len(got))
	}
	if got, _ := l.Peek("s1"); len(got) != 1 {
		t.Fatalf("second peek: want 1 report, got %d", len(got))
	}
	if got := l.Drain("s1", -1); got.Count != 1 {
		t.Fatalf("drain after peeks must still deliver, got %+v", got)
	}
}

func TestSaveLogPendingReported(t *testing.T) {
	l := testLog()
	l.MarkPending("s1", 1045)
	got := l.Drain("s1", -1)
	if got.DueInS != 45 {
		t.Fatalf("want due_in_s 45, got %d", got.DueInS)
	}
	// A finished run supersedes the pending state.
	l.Record(rep("s1", "alpha"))
	if got := l.Drain("s1", -1); got.DueInS != 0 {
		t.Fatalf("record should clear pending, got %d", got.DueInS)
	}
}

func TestSaveLogErrorSurfaces(t *testing.T) {
	l := testLog()
	r := rep("s1")
	r.Skipped = ""
	r.Error = "claude: claude binary not found"
	l.Record(r)
	got := l.Drain("s1", -1)
	if got.Error != "claude: claude binary not found" {
		t.Fatalf("want the error surfaced, got %+v", got)
	}
}

func TestSaveLogSkippedStatus(t *testing.T) {
	l := testLog()
	l.Record(rep("s1"))
	if got := l.Drain("s1", -1); got.Status != "skipped" {
		t.Fatalf("want status skipped, got %q", got.Status)
	}
}

func TestSaveLogSessionCap(t *testing.T) {
	l := testLog()
	for i := 0; i < l.maxSess+10; i++ {
		sid := fmt.Sprintf("s%d", i)
		l.now = func() int64 { return 1000 + int64(i) }
		l.Record(rep(sid, "m"))
	}
	if len(l.bySess) > l.maxSess {
		t.Fatalf("want at most %d sessions, got %d", l.maxSess, len(l.bySess))
	}
}

func TestSaveLogConcurrent(t *testing.T) {
	l := newSaveLog()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("s%d", i%3)
			for j := 0; j < 50; j++ {
				l.Record(rep(sid, "m"))
				l.MarkPending(sid, time.Now().Unix()+45)
				l.Drain(sid, 6)
				l.Peek(sid)
				l.PeekAll(10)
			}
		}(i)
	}
	wg.Wait()
}

func TestSaveLogNilSafe(t *testing.T) {
	var l *saveLog
	l.Record(rep("s1", "alpha"))
	l.MarkPending("s1", 1)
	if got := l.Drain("s1", -1); got.Count != 0 {
		t.Fatal("nil saveLog must drain empty")
	}
	if got, _ := l.Peek("s1"); got != nil {
		t.Fatal("nil saveLog must peek nil")
	}
	if got := l.PeekAll(5); got != nil {
		t.Fatal("nil saveLog must peek-all nil")
	}
}
