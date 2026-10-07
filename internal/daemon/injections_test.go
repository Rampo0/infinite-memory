package daemon

import (
	"fmt"
	"path/filepath"
	"testing"
)

func inj(ids ...string) []injected {
	out := make([]injected, len(ids))
	for i, id := range ids {
		out[i] = injected{ID: id, Title: "t-" + id, Kind: "fact"}
	}
	return out
}

func TestInjectionLogSeenAndReset(t *testing.T) {
	l := newInjectionLog()
	l.Record("s1", inj("a", "b"))
	l.Record("s2", inj("c"))
	if seen := l.Seen("s1"); !seen["a"] || !seen["b"] || seen["c"] {
		t.Fatalf("s1 seen = %v", seen)
	}
	l.Reset("s1")
	if len(l.Seen("s1")) != 0 || !l.Seen("s2")["c"] {
		t.Fatal("reset clears one session only")
	}
}

// B2 feeds the extractor what was shown since its last run.
func TestInjectionLogSince(t *testing.T) {
	l := newInjectionLog()
	now := int64(100)
	l.now = func() int64 { return now }
	l.Record("s1", inj("old"))
	now = 200
	l.Record("s1", inj("new"))
	got := l.Since("s1", 150)
	if len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("want only the injection after 150, got %+v", got)
	}
}

func TestInjectionLogCapsPerSession(t *testing.T) {
	l := newInjectionLog()
	l.maxPerSess = 3
	for i := 0; i < 5; i++ {
		l.Record("s1", inj(fmt.Sprintf("m%d", i)))
	}
	seen := l.Seen("s1")
	if len(seen) != 3 || seen["m0"] || !seen["m4"] {
		t.Fatalf("want the newest 3, got %v", seen)
	}
}

func TestInjectionLogEvictsIdleSessions(t *testing.T) {
	l := newInjectionLog()
	now := int64(0)
	l.now = func() int64 { return now }
	l.Record("idle", inj("a"))
	now = 25 * 3600
	l.Record("busy", inj("b"))
	if len(l.Seen("idle")) != 0 {
		t.Fatal("a session idle past the TTL must be evicted")
	}
}

func TestInjectionLogEmptySessionIsNoOp(t *testing.T) {
	l := newInjectionLog()
	l.Record("", inj("a"))
	if len(l.Seen("")) != 0 {
		t.Fatal("no session id, nothing to track")
	}
	var nilLog *injectionLog
	nilLog.Record("s", inj("a"))
	if nilLog.Seen("s") != nil {
		t.Fatal("a nil log is a no-op")
	}
}

func TestSearchHitsDoNotHideMemoriesFromTheHook(t *testing.T) {
	l := newInjectionLog()
	l.Record("s1", inj("hooked"))
	l.Record("s1", []injected{{ID: "searched", Kind: "fact", Via: viaSearch, Query: "jago whitelist"}})
	if seen := l.Seen("s1"); !seen["hooked"] || seen["searched"] {
		t.Fatalf("only hook injections dedupe the hook; a search may come from a subagent: %v", seen)
	}
	if got := l.Since("s1", 0); len(got) != 2 {
		t.Fatalf("both still go to grading, got %d", len(got))
	}
}

func TestInjectionLogSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "injections.json")
	l := newInjectionLog()
	l.path = path
	l.Record("s1", inj("a"))
	l.flush()
	again := newInjectionLog()
	again.load(path)
	if !again.Seen("s1")["a"] {
		t.Fatal("a daemon restart must not forget what the session was shown")
	}
}
