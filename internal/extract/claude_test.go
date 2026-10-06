package extract

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeClaude installs a stand-in claude that records its argv, one per line,
// and answers the way an isolated spawn does: the schema-shaped JSON under
// plain "result", since --tools "" leaves no StructuredOutput tool to call.
func fakeClaude(t *testing.T) (*Runner, func() []string) {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argsFile + "'\n" +
		"cat >/dev/null\n" +
		`echo '{"result":"{\"memories\":[{\"title\":\"t\",\"content\":\"c\",\"type\":\"rule\"}]}"}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: bin, Model: "m", SpawnDir: filepath.Join(dir, "spawn")}
	return r, func() []string {
		data, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
}

// An isolated extraction strips MCP servers, settings and tools from the
// spawn, and its plain-"result" answer still parses into memories.
func TestRunIsolatedExtraction(t *testing.T) {
	r, args := fakeClaude(t)
	raw, err := r.Run(context.Background(), "transcript", true)
	if err != nil {
		t.Fatal(err)
	}
	got := args()
	for _, want := range []string{"--safe-mode", "--strict-mcp-config", "--setting-sources", "--tools", "--json-schema"} {
		if !slices.Contains(got, want) {
			t.Fatalf("isolated spawn missing %s: %q", want, got)
		}
	}
	if slices.Contains(got, "--allowedTools") {
		t.Fatalf("isolated spawn still allows a tool: %q", got)
	}
	// An empty value is what strips them; a stray value would load some back.
	for _, flag := range []string{"--tools", "--setting-sources"} {
		i := slices.Index(got, flag)
		if i < 0 || i+1 >= len(got) || got[i+1] != "" {
			t.Fatalf("%s must be followed by an empty value: %q", flag, got)
		}
	}
	mems, err := ParseMemories(raw)
	if err != nil || len(mems) != 1 || mems[0].Kind != "rule" {
		t.Fatalf("ParseMemories(%q) = %v, %v", raw, mems, err)
	}
}

// The user's own transcripts keep the interactive shape.
func TestRunInteractiveExtraction(t *testing.T) {
	r, args := fakeClaude(t)
	if _, err := r.Run(context.Background(), "transcript", false); err != nil {
		t.Fatal(err)
	}
	got := args()
	if slices.Contains(got, "--safe-mode") || !slices.Contains(got, "--allowedTools") {
		t.Fatalf("interactive spawn changed shape: %q", got)
	}
}

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
