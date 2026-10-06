package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `imem mcp` end to end: Claude Code starts it over stdio and calls tools,
// which reach the daemon over HTTP.
func TestMCPSearchThroughDaemon(t *testing.T) {
	fd := newFakeDaemon(t)
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"imem_search","arguments":{"query":"jago whitelist","limit":3}}}` + "\n"
	out, code := runCLIStdin(t, addrOf(fd), in, "mcp")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 responses, got %d:\n%s", len(lines), out)
	}
	var res struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &res); err != nil || res.Result.IsError || len(res.Result.Content) != 1 {
		t.Fatalf("bad tools/call response: %s (%v)", lines[1], err)
	}
	text := res.Result.Content[0].Text
	if !strings.Contains(text, "- [fact] Jago whitelist lives in master-data — Whitelist rows sit in master_data.jago_whitelist.") {
		t.Fatalf("search text must carry kind, title and content:\n%s", text)
	}
	q := fd.query("/v1/memories")
	if q.Get("q") != "jago whitelist" || q.Get("limit") != "3" || q.Get("cwd") == "" {
		t.Fatalf("query must reach the daemon with limit and cwd, got %v", q)
	}
}

// Extraction spawns run with INFINITE_MEMORY_INTERNAL=1; an MCP server they
// load must not do anything.
func TestMCPExitsInsideInternalSpawn(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(cfgPath, []byte(`{"http_addr":"127.0.0.1:1"}`), 0o644)
	cmd := exec.Command(os.Args[0], "mcp")
	cmd.Env = append(os.Environ(), "IMEM_CONTRACT_MAIN=1", "IMEM_CONFIG="+cfgPath, "INFINITE_MEMORY_INTERNAL=1")
	cmd.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	out, err := cmd.Output()
	if err != nil || len(out) != 0 {
		t.Fatalf("want a silent exit 0, got %q %v", out, err)
	}
}
