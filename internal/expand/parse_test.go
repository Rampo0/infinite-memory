package expand

import (
	"strings"
	"testing"
)

// knownNames stands in for the index's real entity names.
var knownNames = map[string]bool{
	"guard insert remisier": true,
	"remisier list search":  true,
	"account-remisier":      true,
}

func terms(t *testing.T, raw string, user ...string) []string {
	t.Helper()
	_, got, err := Parse(raw, user, knownNames)
	if err != nil {
		t.Fatalf("Parse(%q) errored: %v", raw, err)
	}
	return got
}

func TestParseStripsFencesAndProse(t *testing.T) {
	cases := map[string]string{
		"plain":  `{"intent":"i","terms":["remisier"]}`,
		"fenced": "```json\n{\"intent\":\"i\",\"terms\":[\"remisier\"]}\n```",
		"prose":  "Here you go:\n{\"intent\":\"i\",\"terms\":[\"remisier\"]}\nhope that helps",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got := terms(t, raw)
			if len(got) != 1 || got[0] != "remisier" {
				t.Fatalf("got %v, want [remisier]", got)
			}
		})
	}
}

func TestParseIntentTrimmed(t *testing.T) {
	intent, _, err := Parse(`{"intent":"  debug the guard  ","terms":[]}`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if intent != "debug the guard" {
		t.Fatalf("intent = %q, want trimmed", intent)
	}
}

func TestParseGarbageErrors(t *testing.T) {
	for _, raw := range []string{"", "not json at all", "[1,2,3]"} {
		if _, _, err := Parse(raw, nil, nil); err == nil {
			t.Fatalf("Parse(%q) should have errored", raw)
		}
	}
}

func TestParseEmptyListIsNotAnError(t *testing.T) {
	if got := terms(t, `{"terms":[]}`); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

// A term the user already typed is not an expansion: counting it would
// inflate the hit count for words that were already being searched.
func TestParseDropsUserTokens(t *testing.T) {
	got := terms(t, `{"terms":["solve","remisier"]}`, "solve")
	if len(got) != 1 || got[0] != "remisier" {
		t.Fatalf("got %v, want [remisier]", got)
	}
}

// A stopword term matches the keyword list of nearly every memory in the
// graph, so it must never reach Cypher however the model produced it.
func TestParseRejectsStopwordTerms(t *testing.T) {
	got := terms(t, `{"terms":["the","of the","issue","guard insert remisier"]}`)
	if len(got) != 1 || got[0] != "guard insert remisier" {
		t.Fatalf("got %v, want only the real term", got)
	}
}

// Structural characters are how an injected payload would try to break out of
// the term list. LLM output is untrusted input.
func TestParseRejectsStructuralChars(t *testing.T) {
	raw := "{\"terms\":[\"ok term\",\"bad{brace\",\"back\\u0060tick\",\"has\\u0001ctrl\"]}"
	got := terms(t, raw)
	if len(got) != 1 || got[0] != "ok term" {
		t.Fatalf("got %v, want only [ok term]", got)
	}
}

func TestParseNormalizesAndDedupes(t *testing.T) {
	got := terms(t, `{"terms":["  Guard   Insert  Remisier ","guard insert remisier","RDN Creation"]}`)
	want := []string{"guard insert remisier", "rdn creation"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Multi-word terms are the precise channel: they match Entity.name_lc exactly,
// so they must survive whole rather than being split into words.
func TestParseKeepsMultiWordIntact(t *testing.T) {
	got := terms(t, `{"terms":["remisier list search"]}`)
	if len(got) != 1 || got[0] != "remisier list search" {
		t.Fatalf("got %v, want the phrase intact", got)
	}
}

// A model that spells an entity as an identifier must still reach the entity.
// "guard-insert-remisier" matches neither Entity.name_lc ("guard insert
// remisier") nor Entity.tokens (["guard","insert","remisier"]), because
// expansion terms bypass textutil.Tokenize.
func TestParseRepairsSpellingIntoKnownName(t *testing.T) {
	got := terms(t, `{"terms":["guard-insert-remisier"]}`)
	want := []string{"guard-insert-remisier", "guard insert remisier"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want the repaired name added %v", got, want)
	}
}

// The repair must only produce names the index actually holds, or it just
// adds a second term that matches nothing.
func TestParseRepairOnlyIntoKnownNames(t *testing.T) {
	if got := terms(t, `{"terms":["some-unknown-thing"]}`); len(got) != 1 {
		t.Fatalf("got %v, want no variant for an unknown name", got)
	}
	if got := terms(t, `{"terms":["remisier"]}`); len(got) != 1 {
		t.Fatalf("got %v, want no variant for a single word", got)
	}
	if got := terms(t, `{"terms":["guard insert remisier"]}`); len(got) != 1 {
		t.Fatalf("got %v, want no duplicate for an already-correct name", got)
	}
}

// The reverse direction: the index holds "account-remisier", so a model that
// writes "account remisier" must still reach it.
func TestParseRepairsPhraseIntoIdentifier(t *testing.T) {
	got := terms(t, `{"terms":["account remisier"]}`)
	want := []string{"account remisier", "account-remisier"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
