package setup

import (
	"fmt"
	"os"
	"strings"

	"github.com/Rampo0/infinite-memory/internal/config"
)

const fixSetup = "run `imem setup` (or `make setup` in the infinite-memory checkout)"

func Doctor(e Env) []Check {
	daemon, memgraph := e.Health()
	return []Check{
		checkClaude(e),
		{Name: "daemon", OK: daemon, Detail: e.Cfg.HTTPAddr, Fix: fixSetup},
		{Name: "memgraph", OK: memgraph, Detail: e.Cfg.MemgraphURI,
			Fix: "start Docker, then `make up` in the infinite-memory checkout"},
		checkHooks(e),
		checkMCP(e),
		checkImport(e),
		checkRulesFile(e),
		checkLaunchd(e),
		checkAgents(e),
	}
}

func checkClaude(e Env) Check {
	bin, err := e.ClaudeBin()
	c := Check{Name: "claude cli", OK: err == nil, Detail: bin,
		Fix: "install Claude Code and run `claude auth login`, or set claude_bin in config.json"}
	if err != nil {
		c.Detail = err.Error()
	}
	return c
}

func checkHooks(e Env) Check {
	data, err := readOptional(e.path(".claude/settings.json"))
	_, changed, merr := MergeSettings(data, e.Exe)
	ok := err == nil && merr == nil && !changed && len(data) > 0
	return Check{Name: "hooks", OK: ok, Detail: "~/.claude/settings.json", Fix: fixSetup}
}

func checkMCP(e Env) Check {
	return Check{Name: "mcp", OK: mcpMatches(currentMCP(e), e.Exe), Detail: "user-scope imem server, alwaysLoad", Fix: fixSetup}
}

func checkImport(e Env) Check {
	data, _ := readOptional(e.path(".claude/CLAUDE.md"))
	_, missing := EnsureLine(data, RulesImport)
	ok := e.Cfg.RulesFile != "" && !missing
	return Check{Name: "rules import", OK: ok, Detail: "rules_file + " + RulesImport + " in ~/.claude/CLAUDE.md", Fix: fixSetup}
}

func checkRulesFile(e Env) Check {
	c := Check{Name: "rules file", Detail: e.Cfg.RulesFile,
		Fix: "start a Claude Code session or restart the daemon; it writes the file"}
	if fi, err := os.Stat(e.Cfg.RulesFile); err == nil && e.Cfg.RulesFile != "" {
		c.OK, c.Detail = true, fmt.Sprintf("%s (%d bytes)", e.Cfg.RulesFile, fi.Size())
	}
	return c
}

func checkLaunchd(e Env) Check {
	cur, _ := readOptional(e.plistPath())
	ok := string(cur) == Plist(e.Home, e.Exe, e.ClaudeDir)
	return Check{Name: "launchd", OK: ok, Detail: e.plistPath(), Fix: fixSetup}
}

func checkAgents(e Env) Check {
	var roots []string
	seen := map[string]bool{}
	for _, r := range append(append([]string(nil), e.Cfg.AgentRoots...), config.DropInRoots(config.AgentsDir())...) {
		if !seen[r] {
			seen[r] = true
			roots = append(roots, r)
		}
	}
	return Check{Name: "agents", OK: true, Detail: fmt.Sprintf("%d agent transcript root(s): %s", len(roots), strings.Join(roots, ", "))}
}
