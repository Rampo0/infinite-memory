package daemon

import (
	"path/filepath"
	"strings"

	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/graph"
)

// fromClaudeProjects reports whether path lies under ~/.claude/projects,
// where Claude Code writes the user's own transcripts.
func fromClaudeProjects(path string) bool {
	root := config.ClaudeProjectsDir() + string(filepath.Separator)
	return strings.HasPrefix(filepath.Clean(path), root)
}

// agentRoot returns the configured agent home holding path, or "" when none
// does. Both sides are symlink-resolved, so a link planted in an agent home
// cannot point the daemon at an arbitrary file.
func agentRoot(path string, roots []string) string {
	if len(roots) == 0 {
		return ""
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err == nil && strings.HasPrefix(real, rr+string(filepath.Separator)) {
			return rr
		}
	}
	return ""
}

// demoteAgentKinds turns rules and preferences into facts. An agent
// transcript is text a bot wrote after reading untrusted input: it may
// inform future sessions but never set their standing rules.
func demoteAgentKinds(mems []graph.MemoryIn) {
	for i := range mems {
		if mems[i].Kind == "rule" || mems[i].Kind == "preference" {
			mems[i].Kind = "fact"
		}
	}
}
