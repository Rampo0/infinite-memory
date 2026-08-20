package extract

import (
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("testdata/sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadDeltaFull(t *testing.T) {
	turns, total, err := ReadDelta(fixture(t), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 9 {
		t.Fatalf("want 9 total lines, got %d", total)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 conversational turns, got %d: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("unexpected roles: %+v", turns)
	}
	if turns[1].Text != "Registered port 7690 as the daemon HTTP address." {
		t.Fatalf("unexpected assistant text: %q", turns[1].Text)
	}
}

func TestReadDeltaCursor(t *testing.T) {
	turns, total, err := ReadDelta(fixture(t), 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 9 {
		t.Fatalf("total should still be 9, got %d", total)
	}
	if len(turns) != 1 || turns[0].Role != "assistant" {
		t.Fatalf("cursor=2 should yield only the assistant turn, got %+v", turns)
	}
}

func TestReadDeltaBudgetKeepsNewest(t *testing.T) {
	turns, _, err := ReadDelta(fixture(t), 0, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Role != "assistant" {
		t.Fatalf("budget should keep only the newest turn, got %+v", turns)
	}
}

func TestReadDeltaMissingFile(t *testing.T) {
	if _, _, err := ReadDelta("/nonexistent/nope.jsonl", 0, 0); err == nil {
		t.Fatal("want error for missing file")
	}
}
