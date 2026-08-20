// Package config loads daemon/CLI configuration: compiled defaults overlaid
// with ~/.config/infinite-memory/config.json (or $IMEM_CONFIG).
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr              string `json:"http_addr"`
	MemgraphURI           string `json:"memgraph_uri"`
	MemgraphUser          string `json:"memgraph_user"`
	MemgraphPass          string `json:"memgraph_pass"`
	ClaudeBin             string `json:"claude_bin"`
	ExtractModel          string `json:"extract_model"`
	RetrieveK             int    `json:"retrieve_k"`
	RetrieveTimeoutMS     int    `json:"retrieve_timeout_ms"`
	DebounceSeconds       int    `json:"debounce_seconds"`
	MaxTranscriptChars    int    `json:"max_transcript_chars"`
	MaxMemoryContentChars int    `json:"max_memory_content_chars"`
	LogFile               string `json:"log_file"`
	// SameProjectBoost is added to scores of memories from the current
	// project; retrieval itself is global (topic-based, not cwd-based).
	SameProjectBoost float64 `json:"same_project_boost"`
	// RulesK caps the always-injected standing-rules section (0 disables).
	RulesK int `json:"rules_k"`
}

func Default() Config {
	return Config{
		HTTPAddr:              "127.0.0.1:7690",
		MemgraphURI:           "bolt://127.0.0.1:7687",
		ClaudeBin:             "claude",
		ExtractModel:          "claude-haiku-4-5-20251001",
		RetrieveK:             6,
		RetrieveTimeoutMS:     300,
		DebounceSeconds:       45,
		MaxTranscriptChars:    24000,
		MaxMemoryContentChars: 400,
		LogFile:               "~/.local/state/infinite-memory/imemd.log",
		SameProjectBoost:      1.0,
		RulesK:                50,
	}
}

// Load returns Default overlaid with the config file, if one exists.
// A broken or missing file never fails: hooks must work with zero setup.
func Load() Config {
	cfg := Default()
	path := os.Getenv("IMEM_CONFIG")
	if path == "" {
		path = ExpandHome("~/.config/infinite-memory/config.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	if cfg.RetrieveK <= 0 {
		cfg.RetrieveK = Default().RetrieveK
	}
	if cfg.DebounceSeconds <= 0 {
		cfg.DebounceSeconds = Default().DebounceSeconds
	}
	if cfg.RulesK < 0 {
		cfg.RulesK = 0
	}
	return cfg
}

func (c Config) BaseURL() string         { return "http://" + c.HTTPAddr }
func (c Config) Debounce() time.Duration { return time.Duration(c.DebounceSeconds) * time.Second }
func (c Config) RetrieveTO() time.Duration {
	if c.RetrieveTimeoutMS <= 0 {
		return 300 * time.Millisecond
	}
	return time.Duration(c.RetrieveTimeoutMS) * time.Millisecond
}

// SpawnDir is the neutral working directory for extraction claude spawns, so
// they never auto-load a project CLAUDE.md.
func (c Config) SpawnDir() string {
	return ExpandHome("~/.local/state/infinite-memory/spawn")
}

func (c Config) LogPath() string { return ExpandHome(c.LogFile) }

// ClaudeProjectsDir is where Claude Code writes transcripts.
func ClaudeProjectsDir() string { return ExpandHome("~/.claude/projects") }

func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
