package project

import (
	"os"
	"path/filepath"
	"testing"
)

func mkdir(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// resolved strips symlinks (macOS temp dirs live under /var -> /private/var).
func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPlainRepoKeyIsItsRoot(t *testing.T) {
	root := resolved(t, t.TempDir())
	mkdir(t, filepath.Join(root, ".git"))
	sub := mkdir(t, filepath.Join(root, "internal", "x"))
	if got := ResolveKey(sub); got != root {
		t.Fatalf("want %s, got %s", root, got)
	}
}

// A linked worktree (`.git` is a file pointing into <main>/.git/worktrees/<n>,
// which holds a commondir file) belongs to its main repository: memories from
// .superset/worktrees/<id>/<branch> must share the repo's project key.
func TestWorktreeResolvesToMainRepo(t *testing.T) {
	base := resolved(t, t.TempDir())
	main := mkdir(t, filepath.Join(base, "opening-account"))
	gitdir := mkdir(t, filepath.Join(main, ".git", "worktrees", "zero-trust-auth"))
	write(t, filepath.Join(gitdir, "commondir"), "../..\n")
	wt := mkdir(t, filepath.Join(base, ".superset", "worktrees", "opening-account", "feature", "zero-trust-auth"))
	write(t, filepath.Join(wt, ".git"), "gitdir: "+gitdir+"\n")
	if got := ResolveKey(mkdir(t, filepath.Join(wt, "pkg"))); got != main {
		t.Fatalf("a worktree must resolve to its main repo %s, got %s", main, got)
	}
}

func TestRelativeGitdirWorktree(t *testing.T) {
	base := resolved(t, t.TempDir())
	main := mkdir(t, filepath.Join(base, "repo"))
	gitdir := mkdir(t, filepath.Join(main, ".git", "worktrees", "wt"))
	write(t, filepath.Join(gitdir, "commondir"), "../..")
	wt := mkdir(t, filepath.Join(base, "wt"))
	write(t, filepath.Join(wt, ".git"), "gitdir: ../repo/.git/worktrees/wt")
	if got := ResolveKey(wt); got != main {
		t.Fatalf("want %s, got %s", main, got)
	}
}

// A submodule's gitdir has no commondir: it is its own project.
func TestSubmoduleStaysItsOwnProject(t *testing.T) {
	base := resolved(t, t.TempDir())
	parent := mkdir(t, filepath.Join(base, "parent"))
	modGit := mkdir(t, filepath.Join(parent, ".git", "modules", "lib"))
	_ = modGit
	lib := mkdir(t, filepath.Join(parent, "lib"))
	write(t, filepath.Join(lib, ".git"), "gitdir: ../.git/modules/lib")
	if got := ResolveKey(lib); got != lib {
		t.Fatalf("a submodule is its own project, want %s got %s", lib, got)
	}
}

func TestNoGitFallsBackToCwd(t *testing.T) {
	dir := resolved(t, t.TempDir())
	if got := ResolveKey(dir); got != dir {
		t.Fatalf("want %s, got %s", dir, got)
	}
}

// Migration of old project keys (additive repo_key). Worktrees still on disk
// resolve directly; a vanished one inherits the repo of a sibling under the
// same .superset/worktrees/<id>/; a segment named like a known repo maps to
// it; anything else needs a manual mapping.
func TestPlanRepoKeys(t *testing.T) {
	ws := "/Users/x/.superset/worktrees/"
	repo := "/Users/x/accountworkspace/registration"
	oa := "/Users/x/accountworkspace/opening-account"
	onDisk := map[string]string{
		ws + "3e48/pollen-boa": repo, // live worktree
		repo:                   repo, // the repo itself
		oa:                     oa,
		"/Users/x/scratch":     "/Users/x/scratch",
	}
	resolve := func(k string) (string, bool) { r, ok := onDisk[k]; return r, ok }
	keys := []string{
		ws + "3e48/pollen-boa", ws + "3e48/hotfix/bca-api-atomicty", // sibling of a live worktree
		ws + "opening-account/feature/zero-trust-auth", // segment named like a known repo
		ws + "8888/lonely",                        // nothing to go on
		ws + "9999/mapped",                        // manual
		ws + "5035/feature/guard-insert-remisier", // group resolved from disk
		repo, "/Users/x/scratch", "/itest/123",
	}
	findRepo := func(group, name string) string {
		switch {
		case name == "opening-account":
			return oa
		case group == ws+"5035/": // a live worktree without memories sits in this group
			return "/Users/x/accountworkspace/account-remisier"
		}
		return ""
	}
	got, unresolved := PlanRepoKeys(keys, resolve, findRepo, map[string]string{ws + "9999/mapped": oa})
	want := map[string]string{
		ws + "3e48/pollen-boa":                         repo,
		ws + "3e48/hotfix/bca-api-atomicty":            repo,
		ws + "opening-account/feature/zero-trust-auth": oa,
		ws + "9999/mapped":                             oa,
		ws + "5035/feature/guard-insert-remisier":      "/Users/x/accountworkspace/account-remisier",
	}
	if len(got) != len(want) {
		t.Fatalf("want %d mappings, got %v", len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: want %s, got %q", k, v, got[k])
		}
	}
	if len(unresolved) != 1 || unresolved[0] != ws+"8888/lonely" {
		t.Fatalf("only the worktree with no evidence is unresolved, got %v", unresolved)
	}
}

func TestPrefixMapsBecomeKeyMaps(t *testing.T) {
	keys := []string{"/Users/old/play/self-agent", "/Users/old/accountworkspace/registration", "/Users/oldish/x", "/other/y"}
	manual := map[string]string{"/other/y": "/other/z"}
	ExpandPrefixMaps(keys, map[string]string{"/Users/old": "/Users/new"}, manual)
	want := map[string]string{
		"/Users/old/play/self-agent":               "/Users/new/play/self-agent",
		"/Users/old/accountworkspace/registration": "/Users/new/accountworkspace/registration",
		"/other/y": "/other/z",
	}
	if len(manual) != len(want) {
		t.Fatalf("got %v", manual)
	}
	for k, v := range want {
		if manual[k] != v {
			t.Fatalf("%s: want %s, got %s (prefix must match whole path segments)", k, v, manual[k])
		}
	}
}

func TestForeignHomeFindsTheOldMachinesHome(t *testing.T) {
	keys := []string{"/Users/old/a", "/Users/old/b/c", "/Users/new/d", "/itest/1", "/Users/old/e"}
	if got := ForeignHome(keys, "/Users/new"); got != "/Users/old" {
		t.Fatalf("got %q", got)
	}
	if got := ForeignHome([]string{"/Users/new/a"}, "/Users/new"); got != "" {
		t.Fatalf("no foreign home, got %q", got)
	}
}
