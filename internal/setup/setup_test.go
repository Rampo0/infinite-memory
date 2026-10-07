package setup

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

const userSettings = `{
  "model": "opus",
  "permissions": {
    "allow": ["Bash(git status)"]
  },
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "rtk-hook"}]}],
    "Stop": [
      {"hooks": [{"type": "command", "command": "double-shot-latte judge"}]},
      {"hooks": [{"type": "command", "command": "/old/path/imem hook stop", "timeout": 60}]}
    ]
  },
  "effortLevel": "xhigh"
}
`

func TestMergeSettingsInstallsImemAndKeepsEverythingElse(t *testing.T) {
	out, changed, err := MergeSettings([]byte(userSettings), "/home/me/.local/bin/imem")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	s := string(out)
	if strings.Index(s, `"model"`) > strings.Index(s, `"permissions"`) || strings.Index(s, `"hooks"`) > strings.Index(s, `"effortLevel"`) {
		t.Fatalf("top-level key order must survive:\n%s", s)
	}
	for _, want := range []string{"rtk-hook", "double-shot-latte judge", "/home/me/.local/bin/imem hook stop",
		"/home/me/.local/bin/imem hook user-prompt", "/home/me/.local/bin/imem hook subagent-start",
		"mcp__imem__imem_search", "Bash(git status)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "/old/path/imem") {
		t.Fatalf("a stale imem hook must be replaced, not kept:\n%s", s)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output must stay valid JSON: %v", err)
	}
}

func TestMergeSettingsAllowsTheRulesFetch(t *testing.T) {
	out, _, err := MergeSettings([]byte(userSettings), "/home/me/.local/bin/imem")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"Bash(imem rules:*)"`, `"Bash(/home/me/.local/bin/imem rules:*)"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("imem rules --here must run without a prompt, missing %s:\n%s", want, out)
		}
	}
}

func TestMergeSettingsIsIdempotent(t *testing.T) {
	once, _, _ := MergeSettings([]byte(userSettings), "/home/me/.local/bin/imem")
	twice, changed, err := MergeSettings(once, "/home/me/.local/bin/imem")
	if err != nil || changed || string(twice) != string(once) {
		t.Fatalf("a second run must change nothing: changed=%v err=%v", changed, err)
	}
}

func TestMergeSettingsFromNothing(t *testing.T) {
	out, changed, err := MergeSettings(nil, "/b/imem")
	if err != nil || !changed || !strings.Contains(string(out), "/b/imem hook session-start") {
		t.Fatalf("an absent settings file gets the hooks: %v %v\n%s", changed, err, out)
	}
}

func TestEnsureImportLine(t *testing.T) {
	out, changed := EnsureLine([]byte("@RTK.md\n"), "@imem-rules.md")
	if !changed || string(out) != "@RTK.md\n@imem-rules.md\n" {
		t.Fatalf("got %q", out)
	}
	if _, changed := EnsureLine(out, "@imem-rules.md"); changed {
		t.Fatal("present already: no change")
	}
}

func TestPlistUsesThisMachinesPaths(t *testing.T) {
	p := Plist("/Users/someone", "/Users/someone/.local/bin/imem", "/opt/homebrew/bin")
	for _, want := range []string{"<string>/Users/someone/.local/bin/imem</string>",
		"/Users/someone/.local/state/infinite-memory/launchd.err.log", "/opt/homebrew/bin:", "<string>com.ammar.imemd</string>"} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "ammardwianwari") {
		t.Fatal("no hard-coded user")
	}
}

func TestMergeSettingsLeavesACanonicalInstallUntouched(t *testing.T) {
	once, _, _ := MergeSettings([]byte(userSettings), "/b/imem")
	moved := strings.Replace(string(once), `"type": "command",`, `"type":   "command",`, 1)
	out, changed, err := MergeSettings([]byte(moved), "/b/imem")
	if err != nil || changed {
		t.Fatalf("whitespace alone is not a change: changed=%v err=%v", changed, err)
	}
	stop := strings.Index(string(out), `"Stop"`)
	judge := strings.Index(string(out)[stop:], "double-shot-latte judge")
	imem := strings.Index(string(out)[stop:], "/b/imem hook stop")
	if judge < 0 || imem < 0 || judge > imem {
		t.Fatalf("hook groups keep their order:\n%s", out)
	}
}

func TestMergeSettingsReplacesAStaleImemHookInPlace(t *testing.T) {
	out, _, _ := MergeSettings([]byte(userSettings), "/b/imem")
	s := string(out)
	stop := strings.Index(s, `"Stop"`)
	judge := strings.Index(s[stop:], "double-shot-latte judge")
	imem := strings.Index(s[stop:], "/b/imem hook stop")
	if judge < 0 || imem < 0 || imem < judge {
		t.Fatalf("the stale group sat after the judge; its replacement must too:\n%s", s)
	}
	if strings.Count(s, "imem hook stop") != 1 {
		t.Fatalf("exactly one imem Stop hook:\n%s", s)
	}
}

const imemFirst = `{
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "/b/imem hook stop", "async": false, "timeout": 120}]},
      {"hooks": [{"type": "command", "command": "notify.sh"}]}
    ]
  }
}
`

func TestMergeSettingsKeepsAnImemGroupWhereItIs(t *testing.T) {
	out, _, err := MergeSettings([]byte(imemFirst), "/b/imem")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Index(s, "/b/imem hook stop") > strings.Index(s, "notify.sh") {
		t.Fatalf("an existing imem group keeps its position:\n%s", s)
	}
	if !regexp.MustCompile(`"type": "command",\s+"command": "/b/imem hook stop"`).MatchString(s) {
		t.Fatalf("a canonical group keeps its own key order:\n%s", s)
	}
}
