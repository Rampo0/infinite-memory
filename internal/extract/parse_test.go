package extract

import (
	"strings"
	"testing"
)

func TestParseMemoriesFenced(t *testing.T) {
	raw := "```json\n{\"memories\":[{\"title\":\"Port choice\",\"content\":\"Daemon uses 7690.\",\"type\":\"decision\",\"entities\":[{\"name\":\"imem daemon\",\"type\":\"component\"}],\"relations\":[[\"imem daemon\",\"uses\",\"memgraph\"]]}]}\n```"
	mems, err := ParseMemories(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 || mems[0].Kind != "decision" || len(mems[0].Entities) != 1 || len(mems[0].Relations) != 1 {
		t.Fatalf("unexpected parse: %+v", mems)
	}
}

func TestParseMemoriesProseWrapped(t *testing.T) {
	raw := "Here is the extraction:\n{\"memories\":[{\"title\":\"t\",\"content\":\"c\",\"type\":\"fact\"}]}\nDone."
	mems, err := ParseMemories(raw)
	if err != nil || len(mems) != 1 {
		t.Fatalf("mems=%v err=%v", mems, err)
	}
}

func TestParseMemoriesValidation(t *testing.T) {
	raw := `{"memories":[
		{"title":"","content":"skipped: empty title","type":"fact"},
		{"title":"bad kind","content":"c","type":"wisdom"},
		{"title":"bad relation","content":"c","type":"fact","relations":[["a","uses"],["a","uses","b"]]},
		{"title":"long","content":"` + strings.Repeat("x", 2000) + `","type":"fact"}
	]}`
	mems, err := ParseMemories(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 3 {
		t.Fatalf("want 3 valid memories, got %d", len(mems))
	}
	if mems[0].Kind != "fact" {
		t.Fatalf("invalid kind should default to fact, got %q", mems[0].Kind)
	}
	if len(mems[1].Relations) != 1 {
		t.Fatalf("malformed relation should be dropped, got %v", mems[1].Relations)
	}
	if len(mems[2].Content) != maxContentChars {
		t.Fatalf("content not clipped: %d", len(mems[2].Content))
	}
}

func TestParseMemoriesGarbage(t *testing.T) {
	if _, err := ParseMemories("total nonsense, no braces"); err == nil {
		t.Fatal("want error on garbage")
	}
}

func TestParseMemoriesEmptyList(t *testing.T) {
	mems, err := ParseMemories(`{"memories":[]}`)
	if err != nil || len(mems) != 0 {
		t.Fatalf("empty list should parse cleanly: mems=%v err=%v", mems, err)
	}
}
