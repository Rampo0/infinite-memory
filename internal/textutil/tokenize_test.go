package textutil

import (
	"reflect"
	"strings"
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want []string
	}{
		{
			name: "compound token emits sub-tokens",
			in:   "How does neo4j-go-driver connect to Memgraph?",
			max:  0,
			want: []string{"neo4j-go-driver", "neo4j", "go", "driver", "connect", "memgraph"},
		},
		{
			name: "stopwords, short tokens and integers dropped",
			in:   "please fix the error in a file 12345 ok?",
			max:  0,
			want: nil,
		},
		{
			name: "dedupe preserves order",
			in:   "daemon daemon port port daemon",
			max:  0,
			want: []string{"daemon", "port"},
		},
		{
			name: "cap applies",
			in:   "alpha beta gamma delta",
			max:  2,
			want: []string{"alpha", "beta"},
		},
		{
			name: "paths split into parts",
			in:   "check internal/graph/save.go",
			max:  0,
			want: []string{"check", "internal/graph/save.go", "internal", "graph", "save", "go"},
		},
		{
			name: "trim edge separators",
			in:   "what is memgraph.",
			max:  0,
			want: []string{"memgraph"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Tokenize(tc.in, tc.max)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Tokenize(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Prompts are often Indonesian: function words and chat filler carry no
// topic and used to crowd the topic words out of the token budget.
func TestIndonesianFillerIsDropped(t *testing.T) {
	// kendala/masalah are Indonesian for issue/problem, which are English
	// stopwords for the same reason: they say nothing about the topic.
	got := Tokenize("setiap request maka ada keyword dari claude, yang ini gimana dong sih udah bisa belum banget bgt kendala masalah", 0)
	want := []string{"request", "keyword", "claude"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// Domain vocabulary must survive: these are what on-call and ai-review
// queries are made of.
func TestIndonesianDomainWordsSurvive(t *testing.T) {
	for _, w := range []string{"rekening", "akun", "nasabah", "dana", "saham", "bank", "pengkinian", "syariah", "menghubungkan", "direject", "wajib", "diisi"} {
		if got := Tokenize(w, 0); len(got) != 1 || got[0] != w {
			t.Fatalf("domain word %q was dropped: %v", w, got)
		}
	}
}
