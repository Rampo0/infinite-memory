// Package textutil provides the shared tokenizer used both when saving
// memories (Memory.keywords, Entity.tokens) and when matching a user prompt
// at retrieval time. One vocabulary on both sides, no drift.
package textutil

import "strings"

func isSep(r rune) bool {
	return r == '_' || r == '.' || r == '/' || r == '-'
}

func isTokenRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || isSep(r)
}

func isInteger(t string) bool {
	for _, r := range t {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(t) > 0
}

// Tokenize lowercases s, splits on non-token runes, and for compound tokens
// (neo4j-go-driver, foo_bar, a/b.c) also emits the sub-parts. Stopwords,
// single-rune tokens and pure integers are dropped; result is deduplicated
// preserving order and capped at max (0 = no cap).
func Tokenize(s string, max int) []string {
	s = strings.ToLower(s)

	var raw []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			raw = append(raw, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		if isTokenRune(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()

	seen := make(map[string]struct{}, len(raw)*2)
	var out []string
	add := func(t string) {
		t = strings.Trim(t, "._/-")
		if len(t) < 2 || isInteger(t) || IsStopword(t) {
			return
		}
		if _, dup := seen[t]; dup {
			return
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}

	for _, t := range raw {
		add(t)
		if strings.ContainsFunc(t, isSep) {
			for _, sub := range strings.FieldsFunc(t, isSep) {
				add(sub)
			}
		}
	}

	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}
