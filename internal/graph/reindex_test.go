package graph

import (
	"strings"
	"testing"
)

// Reindex recomputes token lists from title, content and aliases with the
// current tokenizer, and reports only rows whose lists actually change.
func TestReindexPlan(t *testing.T) {
	rows := []IndexRow{
		{ID: "stale", Title: "Field mask guard", Content: "Returns invalid parameter yang kosong",
			Aliases: []string{"wajib diisi"}, Keywords: []string{"field", "mask", "guard", "yang", "kosong"}},
		{ID: "current", Title: "Port", Content: "Daemon port 7690",
			Keywords: []string{"port", "daemon"}, AliasOnly: nil},
	}
	got := ReindexPlan(rows)
	if len(got) != 1 || got[0].ID != "stale" {
		t.Fatalf("only the stale row changes, got %+v", got)
	}
	kw, ao := strings.Join(got[0].Keywords, ","), strings.Join(got[0].AliasOnly, ",")
	if strings.Contains(kw, "yang") || !strings.Contains(kw, "parameter") || !strings.Contains(kw, "wajib") {
		t.Fatalf("keywords must be retokenized with today's stopwords plus alias tokens: %s", kw)
	}
	if ao != "wajib,diisi" {
		t.Fatalf("alias-only must be the alias tokens the text lacks: %s", ao)
	}
}
