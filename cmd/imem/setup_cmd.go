package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Rampo0/infinite-memory/internal/backup"
	"github.com/Rampo0/infinite-memory/internal/config"
	"github.com/Rampo0/infinite-memory/internal/extract"
	"github.com/Rampo0/infinite-memory/internal/graph"
	"github.com/Rampo0/infinite-memory/internal/project"
	"github.com/Rampo0/infinite-memory/internal/setup"
)

func cmdSetup(args []string) {
	dry, restore := false, ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--dry-run":
			dry = true
		case args[i] == "--restore" && i+1 < len(args):
			restore = args[i+1]
			i++
		}
	}
	e := setupEnv(dry)
	if err := prepareGraph(e, restore); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
	if err := setup.Apply(e); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
	if !dry {
		waitHealthy(e.Cfg, 30*time.Second)
	}
	fmt.Println()
	os.Exit(printDoctor(e, false))
}

func prepareGraph(e setup.Env, restore string) error {
	if e.DryRun {
		if restore != "" {
			fmt.Printf("%-13s would replay %s into %s\n", "restore", restore, e.Cfg.MemgraphURI)
		}
		return nil
	}
	if err := os.MkdirAll(e.Cfg.StateDir(), 0o755); err != nil {
		return err
	}
	if !waitGraph(e.Cfg, 60*time.Second) {
		return fmt.Errorf("memgraph unreachable at %s: start Docker and run `make up` in the infinite-memory checkout", e.Cfg.MemgraphURI)
	}
	if restore == "" {
		return nil
	}
	stmts, err := backup.Load(restore)
	if err != nil || len(stmts) == 0 || !strings.Contains(strings.Join(stmts, "\n"), "__mg_vertex__") {
		return fmt.Errorf("%s does not look like an imem dump (%v)", restore, err)
	}
	_, _ = e.Run("launchctl", "bootout", "gui/"+e.UID+"/"+setup.LaunchdLabel)
	if err := replayDump(e.Cfg, stmts); err != nil {
		return err
	}
	fmt.Printf("%-13s replayed %d statements from %s\n", "restore", len(stmts), restore)
	return remapForeignHome(e.Cfg, e.Home)
}

func remapForeignHome(cfg config.Config, home string) error {
	store, err := graph.New(cfg.MemgraphURI, cfg.MemgraphUser, cfg.MemgraphPass)
	if err != nil {
		return err
	}
	ctx := context.Background()
	defer store.Close(ctx)
	keys, err := store.ProjectKeys(ctx)
	if err != nil {
		return err
	}
	foreign := project.ForeignHome(keys, home)
	if foreign == "" {
		return nil
	}
	mapping := map[string]string{}
	project.ExpandPrefixMaps(keys, map[string]string{foreign: home}, mapping)
	n := 0
	for old, nw := range mapping {
		if _, err := os.Stat(old); err == nil {
			continue
		}
		c, err := store.SetRepoKey(ctx, old, nw)
		if err != nil {
			return err
		}
		n += c
	}
	fmt.Printf("%-13s %d memories moved from %s/… to %s/… (repo_key, additive)\n", "home", n, foreign, home)
	return nil
}

func cmdDoctor(args []string) {
	asJSON := len(args) > 0 && args[0] == "--json"
	os.Exit(printDoctor(setupEnv(false), asJSON))
}

func printDoctor(e setup.Env, asJSON bool) int {
	checks := setup.Doctor(e)
	ok := true
	for _, c := range checks {
		ok = ok && c.OK
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": ok, "checks": checks})
	} else {
		for _, c := range checks {
			mark, fix := "✔", ""
			if !c.OK {
				mark, fix = "✘", " — fix: "+c.Fix
			}
			fmt.Printf("%s %-13s %s%s\n", mark, c.Name, c.Detail, fix)
		}
	}
	if !ok {
		return 1
	}
	return 0
}

func setupEnv(dry bool) setup.Env {
	cfg := config.Load()
	home, _ := os.UserHomeDir()
	runner := extract.NewRunner(cfg)
	claude, _ := runner.ResolveBin()
	return setup.Env{
		Home: home, Exe: installedExe(home), ConfigPath: config.Path(), ClaudeDir: extraDir(claude, home),
		UID: strconv.Itoa(os.Getuid()), DryRun: dry, Run: runCommand, Out: os.Stdout, Cfg: cfg,
		Health: func() (bool, bool) { return daemonHealth(cfg) }, ClaudeBin: runner.ResolveBin,
	}
}

func installedExe(home string) string {
	installed := filepath.Join(home, ".local", "bin", "imem")
	if _, err := os.Stat(installed); err == nil {
		return installed
	}
	exe, err := os.Executable()
	if err != nil {
		return installed
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

func extraDir(claude, home string) string {
	if claude == "" {
		return ""
	}
	dir := filepath.Dir(claude)
	for _, d := range []string{"/usr/local/bin", "/usr/bin", "/bin", filepath.Join(home, ".local", "bin")} {
		if dir == d {
			return ""
		}
	}
	return dir
}

func runCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func daemonHealth(cfg config.Config) (bool, bool) {
	var h map[string]any
	if err := getJSON(cfg.BaseURL()+"/healthz", &h); err != nil {
		return false, false
	}
	mg, _ := h["memgraph"].(bool)
	return true, mg
}

func waitHealthy(cfg config.Config, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if up, _ := daemonHealth(cfg); up {
			return
		}
		time.Sleep(time.Second)
	}
}

func waitGraph(cfg config.Config, limit time.Duration) bool {
	store, err := graph.New(cfg.MemgraphURI, cfg.MemgraphUser, cfg.MemgraphPass)
	if err != nil {
		return false
	}
	defer store.Close(context.Background())
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := store.Ping(ctx)
		cancel()
		if err == nil {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}
