// Package project maps a hook cwd to a stable project key: the git root
// containing it, so subdirectory sessions share the repo's memory.
package project

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var cache sync.Map // abs cwd -> project key

// ResolveKey walks up from cwd looking for a .git entry and returns that
// directory — or, for a linked worktree, its main repository, so every
// .superset/worktrees/<id>/<branch> checkout of a repo shares one project.
// Falls back to cwd itself when no .git is found.
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
		if fi, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			key = d
			if !fi.IsDir() {
				if main := worktreeMain(d); main != "" {
					key = main
				}
			}
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

// worktreeMain returns the main repository of a linked worktree rooted at
// dir, or "" when dir is not one. A worktree's .git file says
// "gitdir: <main>/.git/worktrees/<name>", and that gitdir holds a commondir
// file pointing at <main>/.git. Submodules have a .git file too, but their
// gitdir carries no commondir: they stay their own project.
func worktreeMain(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	common, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return ""
	}
	cd := strings.TrimSpace(string(common))
	if !filepath.IsAbs(cd) {
		cd = filepath.Join(gitdir, cd)
	}
	cd = filepath.Clean(cd)
	if filepath.Base(cd) != ".git" {
		return "" // a bare or unusual layout: keep the worktree's own key
	}
	return filepath.Dir(cd)
}

const worktreesDir = "/.superset/worktrees/"

// worktreeGroup is the ".superset/worktrees/<id>/" prefix of a key, or "".
func worktreeGroup(k string) (group, segment string) {
	i := strings.Index(k, worktreesDir)
	if i < 0 {
		return "", ""
	}
	rest := k[i+len(worktreesDir):]
	seg, _, _ := strings.Cut(rest, "/")
	if seg == "" {
		return "", ""
	}
	return k[:i+len(worktreesDir)] + seg + "/", seg
}

// PlanRepoKeys maps old project keys to their repository root, for the
// additive repo_key migration. resolve reports a key's root and whether its
// path still exists. Only keys whose root differs are returned. A vanished
// worktree takes the manual mapping if any, else the root of a live sibling
// key under the same .superset/worktrees/<id>/ (when they all agree), else
// whatever findRepo(group, id) locates on disk — a live worktree in the group
// that has no memories, or a repository named like the id (repo-named groups
// such as worktrees/master-data/...); the rest come back as unresolved.
func PlanRepoKeys(keys []string, resolve func(string) (string, bool), findRepo func(group, name string) string,
	manual map[string]string) (map[string]string, []string) {
	out := map[string]string{}
	groupRoots := map[string]map[string]bool{}
	rootsByBase := map[string]map[string]bool{}
	var gone []string
	for _, k := range keys {
		root, ok := resolve(k)
		if !ok {
			gone = append(gone, k)
			continue
		}
		if root != k {
			out[k] = root
		}
		if g, _ := worktreeGroup(k); g != "" {
			if groupRoots[g] == nil {
				groupRoots[g] = map[string]bool{}
			}
			groupRoots[g][root] = true
		}
		base := filepath.Base(root)
		if rootsByBase[base] == nil {
			rootsByBase[base] = map[string]bool{}
		}
		rootsByBase[base][root] = true
	}
	only := func(set map[string]bool) string {
		if len(set) != 1 {
			return ""
		}
		for r := range set {
			return r
		}
		return ""
	}
	var unresolved []string
	for _, k := range gone {
		if m, ok := manual[k]; ok {
			out[k] = m
			continue
		}
		g, seg := worktreeGroup(k)
		if g == "" {
			continue // not a worktree: nothing to migrate
		}
		if r := only(groupRoots[g]); r != "" {
			out[k] = r
			continue
		}
		if r := only(rootsByBase[seg]); r != "" {
			out[k] = r
			continue
		}
		if findRepo != nil {
			if r := findRepo(g, seg); r != "" {
				out[k] = r
				continue
			}
		}
		unresolved = append(unresolved, k)
	}
	return out, unresolved
}

func ExpandPrefixMaps(keys []string, prefixes, manual map[string]string) {
	for _, k := range keys {
		if _, ok := manual[k]; ok {
			continue
		}
		for old, nw := range prefixes {
			if k == old || strings.HasPrefix(k, old+"/") {
				manual[k] = nw + strings.TrimPrefix(k, old)
				break
			}
		}
	}
}

func ForeignHome(keys []string, home string) string {
	counts := map[string]int{}
	for _, k := range keys {
		if h := homeOf(k); h != "" && h != home {
			counts[h]++
		}
	}
	best, n := "", 0
	for h, c := range counts {
		if c > n || (c == n && h < best) {
			best, n = h, c
		}
	}
	return best
}

func homeOf(key string) string {
	for _, base := range []string{"/Users/", "/home/"} {
		if rest, ok := strings.CutPrefix(key, base); ok {
			if user, _, _ := strings.Cut(rest, "/"); user != "" {
				return base + user
			}
		}
	}
	return ""
}
