package textutil

import (
	"reflect"
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
