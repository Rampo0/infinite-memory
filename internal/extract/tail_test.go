package extract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeTranscript(t *testing.T, lines ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		data, _ := json.Marshal(l)
		b.Write(data)
		b.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func msg(role, text string) map[string]any {
	return map[string]any{"type": role, "message": map[string]any{"role": role,
		"content": []map[string]any{{"type": "text", "text": text}}}}
}

func TestReadTailKeepsOnlyWholeLinesInTheWindow(t *testing.T) {
	path := writeTranscript(t,
		msg("user", "old question about "+strings.Repeat("x", 400)),
		msg("user", "verify the imem retrieval spec"),
		msg("assistant", "imem hooks inject memories per prompt"),
	)
	turns, err := ReadTail(path, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Text != "verify the imem retrieval spec" {
		t.Fatalf("want the 2 newest whole turns, got %+v", turns)
	}
}

func TestLastExchangePutsTheUserTopicFirst(t *testing.T) {
	turns := []Turn{
		{Role: "user", Text: "first"},
		{Role: "user", Text: "verify imem spec"},
		{Role: "assistant", Text: "hooks inject memories"},
	}
	if got := LastExchange(turns, 0); got != "verify imem spec\nhooks inject memories" {
		t.Fatalf("got %q", got)
	}
}

func TestLastExchangeClipsOnARuneBoundary(t *testing.T) {
	got := LastExchange([]Turn{{Role: "user", Text: strings.Repeat("é", 10)}}, 5)
	if !utf8.ValidString(got) || len(got) > 5 {
		t.Fatalf("clip must stay valid UTF-8 within the cap, got %q", got)
	}
}

func TestReadTailMissingFile(t *testing.T) {
	if _, err := ReadTail(filepath.Join(t.TempDir(), "none.jsonl"), 100); err == nil {
		t.Fatal("a missing transcript must be an error")
	}
}
