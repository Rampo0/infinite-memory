package expand

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/Rampo0/infinite-memory/internal/extract"
	"github.com/Rampo0/infinite-memory/internal/textutil"
)

type rawExpansion struct {
	Intent string   `json:"intent"`
	Terms  []string `json:"terms"`
}

// Parse defensively reads the model's expansion. There are no numeric caps
// here — ExpansionSchema already bounds count and length CLI-side. What is
// left is hygiene, and every rule below exists because LLM output is untrusted
// input that flows straight into a retrieval query.
//
// userTokens are the prompt's own tokens; a "term" equal to one of them is not
// an expansion and is dropped. known is the set of real entity names (lowercase)
// and may be nil; it is used only to repair a term the model spelled in the
// wrong style.
func Parse(raw string, userTokens []string, known map[string]bool) (intent string, terms []string, err error) {
	body, err := extract.SliceJSON(raw)
	if err != nil {
		return "", nil, err
	}
	var parsed rawExpansion
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return "", nil, err
	}

	seen := make(map[string]bool, len(userTokens)+len(parsed.Terms))
	for _, t := range userTokens {
		seen[t] = true
	}
	add := func(t string) {
		if t == "" || seen[t] || !usableTerm(t) {
			return
		}
		seen[t] = true
		terms = append(terms, t)
	}
	for _, t := range parsed.Terms {
		t = normalizeTerm(t)
		add(t)
		// Models spell a multi-word entity as an identifier
		// ("guard-insert-remisier") about as often as they spell it the way
		// the index stores it ("guard insert remisier"), and the wrong style
		// matches neither Entity.name_lc nor Entity.tokens. Repair it — but
		// only into a name the index actually holds, so a mis-spelled guess
		// never becomes a second dead term.
		for _, alt := range respellings(t) {
			if known[alt] {
				add(alt)
			}
		}
	}
	return strings.TrimSpace(parsed.Intent), terms, nil
}

// normalizeTerm lowercases and collapses internal whitespace so a multi-word
// term lines up with Entity.name_lc, which graph.SaveBatch builds the same way.
func normalizeTerm(t string) string {
	return strings.ToLower(strings.Join(strings.Fields(t), " "))
}

// respellings returns the same term in the other separator style: spaced for
// an identifier, hyphenated for a phrase.
func respellings(t string) []string {
	if strings.ContainsAny(t, "-_/.") {
		return []string{strings.Join(strings.FieldsFunc(t, isIdentSep), " ")}
	}
	if fields := strings.Fields(t); len(fields) > 1 {
		return []string{strings.Join(fields, "-"), strings.Join(fields, "_")}
	}
	return nil
}

func isIdentSep(r rune) bool { return r == '-' || r == '_' || r == '/' || r == '.' }

// usableTerm rejects the two shapes that would actively harm retrieval:
// structural characters (an injected payload trying to escape the term list)
// and terms made entirely of stopwords — a term like "the" is present in the
// keyword list of nearly every memory in the graph.
func usableTerm(t string) bool {
	if len(t) < 2 {
		return false
	}
	for _, r := range t {
		if unicode.IsControl(r) || strings.ContainsRune("{}`\"'\\", r) {
			return false
		}
	}
	for _, w := range strings.Fields(t) {
		if !textutil.IsStopword(w) {
			return true
		}
	}
	return false
}
