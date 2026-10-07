package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentsAddListRemove(t *testing.T) {
	fd := newFakeDaemon(t)
	cfgDir := t.TempDir()
	env := []string{"IMEM_CONFIG=" + filepath.Join(cfgDir, "config.json")}
	run := func(args ...string) (string, int) {
		return runCLIWith(t, cliCall{addr: addrOf(fd), env: env}, args...)
	}
	if _, code := run("agents", "add", "test-bot", "~/.test-bot/imem"); code != 0 {
		t.Fatalf("add exited %d", code)
	}
	data, err := os.ReadFile(filepath.Join(cfgDir, "agents.d", "test-bot.json"))
	if err != nil || !strings.Contains(string(data), `"root": "~/.test-bot/imem"`) {
		t.Fatalf("add must write the drop-in, got %q (%v)", data, err)
	}
	if out, _ := run("agents", "list"); !strings.Contains(out, "test-bot") || !strings.Contains(out, ".test-bot/imem") {
		t.Fatalf("list must show the agent, got %q", out)
	}
	if _, code := run("agents", "add", "../evil", "/tmp"); code == 0 {
		t.Fatal("a name that is not a plain identifier must be refused")
	}
	run("agents", "remove", "test-bot")
	if _, err := os.Stat(filepath.Join(cfgDir, "agents.d", "test-bot.json")); !os.IsNotExist(err) {
		t.Fatal("remove must delete the drop-in")
	}
}

func TestDoctorJSONContract(t *testing.T) {
	fd := newFakeDaemon(t)
	home := t.TempDir()
	out, code := runCLIWith(t, cliCall{addr: addrOf(fd), env: []string{"HOME=" + home}}, "doctor", "--json")
	var got struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
			Fix  string `json:"fix"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("doctor --json must print one JSON object: %v\n%s", err, out)
	}
	if got.OK || code != 1 {
		t.Fatalf("an empty home is not a working install: ok=%v code=%d", got.OK, code)
	}
	names := map[string]bool{}
	for _, c := range got.Checks {
		names[c.Name] = c.OK
		if !c.OK && c.Fix == "" {
			t.Fatalf("%s failed without a fix", c.Name)
		}
	}
	if !names["daemon"] || names["hooks"] {
		t.Fatalf("daemon reachable, hooks missing in an empty home: %v", names)
	}
}
