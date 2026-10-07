package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Rampo0/infinite-memory/internal/config"
)

const (
	RulesImport      = "@imem-rules.md"
	RulesFileDefault = "~/.claude/imem-rules.md"
)

type Env struct {
	Home       string
	Exe        string
	ConfigPath string
	ClaudeDir  string
	UID        string
	DryRun     bool
	Run        func(name string, args ...string) (string, error)
	Out        io.Writer
	Cfg        config.Config
	Health     func() (bool, bool)
	ClaudeBin  func() (string, error)
}

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

type step struct {
	name string
	run  func(e Env) (string, error)
}

func Apply(e Env) error {
	steps := []step{{"config", ensureConfig}, {"hooks", ensureSettings}, {"rules import", ensureImport},
		{"mcp", ensureMCP}, {"launchd", ensureLaunchd}}
	for _, s := range steps {
		msg, err := s.run(e)
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		report(e, s.name, msg)
	}
	return nil
}

func report(e Env, name, msg string) {
	switch {
	case msg == "":
		msg = "ok"
	case e.DryRun:
		msg = "would " + msg
	}
	fmt.Fprintf(e.Out, "%-13s %s\n", name, msg)
}

func (e Env) path(rel string) string { return filepath.Join(e.Home, rel) }

func readOptional(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

func writeWithBackup(e Env, path string, data []byte, mode os.FileMode) error {
	if e.DryRun {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		bak := path + ".bak-imem-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(bak, old, mode); err != nil {
			return err
		}
	}
	tmp := path + ".imem-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ensureConfig(e Env) (string, error) {
	data, err := readOptional(e.ConfigPath)
	if err != nil {
		return "", err
	}
	fields, err := parseObject(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", e.ConfigPath, err)
	}
	var cur string
	_ = json.Unmarshal(get(fields, "rules_file"), &cur)
	if cur != "" {
		return "", nil
	}
	val, _ := marshal(RulesFileDefault)
	out, err := writeObject(set(fields, "rules_file", val))
	if err != nil {
		return "", err
	}
	return "set rules_file to " + RulesFileDefault + " in " + e.ConfigPath, writeWithBackup(e, e.ConfigPath, out, 0o644)
}

func ensureSettings(e Env) (string, error) {
	path := e.path(".claude/settings.json")
	data, err := readOptional(path)
	if err != nil {
		return "", err
	}
	out, changed, err := MergeSettings(data, e.Exe)
	if err != nil || !changed {
		return "", err
	}
	return "register the imem hooks and allow imem_search and imem rules in " + path, writeWithBackup(e, path, out, 0o644)
}

func ensureImport(e Env) (string, error) {
	path := e.path(".claude/CLAUDE.md")
	data, err := readOptional(path)
	if err != nil {
		return "", err
	}
	out, changed := EnsureLine(data, RulesImport)
	if !changed {
		return "", nil
	}
	return "import " + RulesImport + " in " + path, writeWithBackup(e, path, out, 0o644)
}
