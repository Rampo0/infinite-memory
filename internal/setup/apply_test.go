package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rampo0/infinite-memory/internal/config"
)

type execLog struct{ calls []string }

func (l *execLog) run(name string, args ...string) (string, error) {
	l.calls = append(l.calls, name+" "+strings.Join(args, " "))
	return "", nil
}

func testEnv(t *testing.T) (Env, *execLog) {
	t.Helper()
	home := t.TempDir()
	log := &execLog{}
	return Env{
		Home: home, Exe: filepath.Join(home, ".local/bin/imem"), ConfigPath: filepath.Join(home, ".config/infinite-memory/config.json"),
		Run: log.run, ClaudeDir: "/opt/homebrew/bin", UID: "501", Out: &bytes.Buffer{},
	}, log
}

func read(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestApplyInstallsEverythingOnAFreshHome(t *testing.T) {
	e, log := testEnv(t)
	if err := Apply(e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, e.ConfigPath), `"rules_file": "~/.claude/imem-rules.md"`) {
		t.Fatal("a fresh config must route rules through the rules file")
	}
	if !strings.Contains(read(t, filepath.Join(e.Home, ".claude/settings.json")), e.Exe+" hook stop") {
		t.Fatal("hooks must be registered")
	}
	if strings.TrimSpace(read(t, filepath.Join(e.Home, ".claude/CLAUDE.md"))) != "@imem-rules.md" {
		t.Fatal("CLAUDE.md must import the rules file")
	}
	if !strings.Contains(read(t, filepath.Join(e.Home, "Library/LaunchAgents", LaunchdLabel+".plist")), e.Exe) {
		t.Fatal("the launchd job must point at this binary")
	}
	joined := strings.Join(log.calls, "\n")
	for _, want := range []string{`claude mcp add-json imem {"alwaysLoad":true,"args":["mcp"],"command":"` + e.Exe + `","type":"stdio"} -s user`,
		"launchctl bootstrap gui/501 " + filepath.Join(e.Home, "Library/LaunchAgents", LaunchdLabel+".plist")} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing call %q in:\n%s", want, joined)
		}
	}
}

func TestApplyTwiceOnlyRestartsTheDaemon(t *testing.T) {
	e, _ := testEnv(t)
	if err := Apply(e); err != nil {
		t.Fatal(err)
	}
	writeMCP(t, e)
	log := &execLog{}
	e.Run = log.run
	if err := Apply(e); err != nil {
		t.Fatal(err)
	}
	if len(log.calls) != 1 || !strings.HasPrefix(log.calls[0], "launchctl kickstart -k gui/501/"+LaunchdLabel) {
		t.Fatalf("a second run changes nothing but restarts the daemon onto the new binary: %v", log.calls)
	}
}

func TestApplyKeepsAnExistingConfig(t *testing.T) {
	e, _ := testEnv(t)
	_ = os.MkdirAll(filepath.Dir(e.ConfigPath), 0o755)
	_ = os.WriteFile(e.ConfigPath, []byte(`{"extract_model": "claude-opus-5", "retrieve_k": -1}`), 0o644)
	if err := Apply(e); err != nil {
		t.Fatal(err)
	}
	got := read(t, e.ConfigPath)
	if !strings.Contains(got, `"extract_model": "claude-opus-5"`) || !strings.Contains(got, `"rules_file"`) {
		t.Fatalf("existing choices stay, the rules file is added:\n%s", got)
	}
}

func TestApplyDryRunTouchesNothing(t *testing.T) {
	e, log := testEnv(t)
	e.DryRun = true
	if err := Apply(e); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.Home, ".claude")); !os.IsNotExist(err) || len(log.calls) != 0 {
		t.Fatalf("dry run wrote files or ran %v", log.calls)
	}
	if out := e.Out.(*bytes.Buffer).String(); !strings.Contains(out, "would") {
		t.Fatalf("dry run must say what it would do:\n%s", out)
	}
}

func writeMCP(t *testing.T, e Env) {
	t.Helper()
	body := `{"mcpServers": {"imem": {"type": "stdio", "command": "` + e.Exe + `", "args": ["mcp"], "alwaysLoad": true}}}`
	if err := os.WriteFile(filepath.Join(e.Home, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorPassesACompleteInstall(t *testing.T) {
	e, _ := testEnv(t)
	_ = Apply(e)
	writeMCP(t, e)
	_ = os.WriteFile(filepath.Join(e.Home, ".claude/imem-rules.md"), []byte("# rules\n"), 0o644)
	e.Cfg = config.Config{RulesFile: filepath.Join(e.Home, ".claude/imem-rules.md")}
	e.Health = func() (bool, bool) { return true, true }
	e.ClaudeBin = func() (string, error) { return "/opt/homebrew/bin/claude", nil }
	for _, c := range Doctor(e) {
		if !c.OK {
			t.Fatalf("%s failed: %s (fix: %s)", c.Name, c.Detail, c.Fix)
		}
	}
}

func TestDoctorNamesTheFixForAMissingPiece(t *testing.T) {
	e, _ := testEnv(t)
	e.Health = func() (bool, bool) { return false, false }
	e.ClaudeBin = func() (string, error) { return "", os.ErrNotExist }
	failed := map[string]string{}
	for _, c := range Doctor(e) {
		if !c.OK {
			failed[c.Name] = c.Fix
		}
	}
	for _, name := range []string{"daemon", "hooks", "mcp", "rules import", "launchd", "claude cli"} {
		if failed[name] == "" {
			t.Fatalf("%s must fail with a fix on an empty home, got %v", name, failed)
		}
	}
}

func TestApplyBootstrapsAJobThatIsNotLoaded(t *testing.T) {
	e, _ := testEnv(t)
	if err := Apply(e); err != nil {
		t.Fatal(err)
	}
	writeMCP(t, e)
	var calls []string
	e.Run = func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] == "kickstart" {
			return "Could not find service", os.ErrNotExist
		}
		return "", nil
	}
	if err := Apply(e); err != nil {
		t.Fatalf("an unloaded job must be bootstrapped, not fail: %v", err)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[1], "launchctl bootstrap gui/501 ") {
		t.Fatalf("want kickstart then bootstrap, got %v", calls)
	}
}

func TestApplyKeepsACustomRulesFile(t *testing.T) {
	e, _ := testEnv(t)
	_ = os.MkdirAll(filepath.Dir(e.ConfigPath), 0o755)
	_ = os.WriteFile(e.ConfigPath, []byte(`{"rules_file": "~/notes/rules.md"}`), 0o644)
	if err := Apply(e); err != nil {
		t.Fatal(err)
	}
	if got := read(t, e.ConfigPath); got != `{"rules_file": "~/notes/rules.md"}` {
		t.Fatalf("a rules_file the user chose must be left alone, got %q", got)
	}
}

func TestDoctorFlagsSettingsWithoutImemHooks(t *testing.T) {
	e, _ := testEnv(t)
	_ = os.MkdirAll(e.path(".claude"), 0o755)
	_ = os.WriteFile(e.path(".claude/settings.json"), []byte(`{"model": "opus"}`), 0o644)
	e.Health = func() (bool, bool) { return true, true }
	e.ClaudeBin = func() (string, error) { return "/x/claude", nil }
	for _, c := range Doctor(e) {
		if c.Name == "hooks" && c.OK {
			t.Fatal("a settings file without the imem hooks is not an install")
		}
	}
}
