package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func wantMCP(exe string) map[string]any {
	return map[string]any{"type": "stdio", "command": exe, "args": []any{"mcp"}, "alwaysLoad": true}
}

func currentMCP(e Env) map[string]any {
	data, err := os.ReadFile(e.path(".claude.json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return nil
	}
	return cfg.MCPServers["imem"]
}

func mcpMatches(cur map[string]any, exe string) bool {
	if cur == nil {
		return false
	}
	for k, v := range wantMCP(exe) {
		a, _ := marshal(cur[k])
		b, _ := marshal(v)
		if string(a) != string(b) {
			return false
		}
	}
	return true
}

func ensureMCP(e Env) (string, error) {
	cur := currentMCP(e)
	if mcpMatches(cur, e.Exe) {
		return "", nil
	}
	if e.DryRun {
		return "register the imem MCP server (alwaysLoad) at user scope", nil
	}
	if cur != nil {
		if _, err := e.Run("claude", "mcp", "remove", "imem", "-s", "user"); err != nil {
			return "", err
		}
	}
	spec, err := marshal(wantMCP(e.Exe))
	if err != nil {
		return "", err
	}
	if _, err := e.Run("claude", "mcp", "add-json", "imem", string(spec), "-s", "user"); err != nil {
		return "", err
	}
	return "register the imem MCP server (alwaysLoad) at user scope", nil
}

func (e Env) plistPath() string {
	return e.path(filepath.Join("Library/LaunchAgents", LaunchdLabel+".plist"))
}

func ensureLaunchd(e Env) (string, error) {
	want := Plist(e.Home, e.Exe, e.ClaudeDir)
	cur, err := readOptional(e.plistPath())
	if err != nil {
		return "", err
	}
	target := "gui/" + e.UID + "/" + LaunchdLabel
	if string(cur) == want {
		if e.DryRun {
			return "", nil
		}
		if _, err := e.Run("launchctl", "kickstart", "-k", target); err == nil {
			return "", nil
		}
		if _, err := e.Run("launchctl", "bootstrap", "gui/"+e.UID, e.plistPath()); err != nil {
			return "", err
		}
		return "load the launchd job", nil
	}
	if e.DryRun {
		return "install the launchd job " + e.plistPath(), nil
	}
	if err := os.MkdirAll(e.path(".local/state/infinite-memory"), 0o755); err != nil {
		return "", err
	}
	if err := writeWithBackup(e, e.plistPath(), []byte(want), 0o644); err != nil {
		return "", err
	}
	_, _ = e.Run("launchctl", "bootout", target)
	if _, err := e.Run("launchctl", "bootstrap", "gui/"+e.UID, e.plistPath()); err != nil {
		return "", err
	}
	return "install the launchd job " + e.plistPath(), nil
}
