package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// imem eval --json is what before/after comparisons diff, so its shape is
// pinned: per-case results plus a per-mode summary.
func TestEvalJSONOutput(t *testing.T) {
	fd := newFakeDaemon(t)
	cases := filepath.Join(t.TempDir(), "eval.jsonl")
	data := `{"name":"h1","mode":"hook","prompt":"jago syariah","cwd":"/c","expect":["jago syariah binds"]}` + "\n" +
		`{"name":"s1","mode":"search","prompt":"jago whitelist","cwd":"/c","expect":["never returned"]}` + "\n"
	if err := os.WriteFile(cases, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runCLI(t, addrOf(fd), "eval", "--file", cases, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var got struct {
		Results []struct {
			Name  string `json:"name"`
			Score struct {
				Hit  bool `json:"hit"`
				Rank int  `json:"rank"`
			} `json:"score"`
		} `json:"results"`
		Summary map[string]struct {
			N       int     `json:"n"`
			HitRate float64 `json:"hit_rate"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("eval --json is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 2 || !got.Results[0].Score.Hit || got.Results[0].Score.Rank != 1 || got.Results[1].Score.Hit {
		t.Fatalf("per-case scores wrong: %+v", got.Results)
	}
	if got.Summary["hook"].N != 1 || got.Summary["hook"].HitRate != 1 || got.Summary["search"].HitRate != 0 {
		t.Fatalf("summary wrong: %+v", got.Summary)
	}
}
