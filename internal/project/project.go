// Package project maps a hook cwd to a stable project key: the git root
// containing it, so subdirectory sessions share the repo's memory.
package project

import (
	"os"
	"path/filepath"
	"sync"
)

var cache sync.Map // abs cwd -> project key

// ResolveKey walks up from cwd looking for a .git entry (dir or file, so
// worktrees count) and returns that directory; falls back to cwd itself.
func ResolveKey(cwd string) string {
	if cwd == "" {
		return "unknown"
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return cwd
	}
	if v, ok := cache.Load(abs); ok {
		return v.(string)
	}
	key := abs
	for d := abs; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			key = d
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	cache.Store(abs, key)
	return key
}
