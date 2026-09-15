package extract

import "testing"

func TestParseEnvelope(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
		err  bool
	}{
		{"structured_result object", `{"structured_result":{"memories":[]}}`, `{"memories":[]}`, false},
		{"structured_result string", `{"structured_result":"{\"memories\":[]}"}`, `{"memories":[]}`, false},
		{"structured_output", `{"structured_output":{"a":1}}`, `{"a":1}`, false},
		// --tools "" produces no StructuredOutput tool call, so a schema-shaped
		// answer arrives under plain "result" instead. The expansion path
		// depends on this.
		{"plain result", `{"result":"{\"terms\":[]}"}`, `{"terms":[]}`, false},
		{"skips empty result", `{"structured_result":"","result":"{\"a\":1}"}`, `{"a":1}`, false},
		{"is_error", `{"is_error":true,"result":"boom"}`, "", true},
		{"not json", `nope`, "", true},
		{"no usable key", `{"session_id":"x"}`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseEnvelope([]byte(c.out))
			if c.err {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestSliceJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                 `{"a":1}`,
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"here:\n{\"a\":1}\nbye":   `{"a":1}`,
	}
	for raw, want := range cases {
		got, err := SliceJSON(raw)
		if err != nil {
			t.Fatalf("SliceJSON(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("SliceJSON(%q) = %q, want %q", raw, got, want)
		}
	}
	if _, err := SliceJSON("no object here"); err == nil {
		t.Fatal("want error when there is no JSON object")
	}
}
