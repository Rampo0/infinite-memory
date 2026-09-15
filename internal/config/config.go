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
	HTTPAddr     string `json:"http_addr"`
	MemgraphURI  string `json:"memgraph_uri"`
	MemgraphUser string `json:"memgraph_user"`
	MemgraphPass string `json:"memgraph_pass"`
	ClaudeBin    string `json:"claude_bin"`
	ExtractModel string `json:"extract_model"`
	// ExpandEnabled turns on the LLM query expander on the retrieval path.
	// Note the polarity: every other bool here defaults true and relies on
	// "explicit false wins". This one defaults false and relies on "explicit
	// true wins" — same unmarshal-over-defaults mechanism, opposite direction,
	// because expansion adds seconds to every prompt.
	ExpandEnabled bool `json:"expand_enabled"`
	// ExpandModel is deliberately separate from ExtractModel: extraction runs
	// a big model over a whole transcript off the critical path, expansion
	// runs a small one while the user waits.
	ExpandModel string `json:"expand_model"`
	// ExpandBudgetMS caps the expansion spawn. It is spent BEFORE the
	// RetrieveTimeoutMS graph budget, not inside it.
	ExpandBudgetMS int `json:"expand_budget_ms"`
	// RetrieveK caps the keyword/entity-matched memories per prompt.
	// -1 means no limit; 0 falls back to the default.
	RetrieveK             int    `json:"retrieve_k"`
	RetrieveTimeoutMS     int    `json:"retrieve_timeout_ms"`
	DebounceSeconds       int    `json:"debounce_seconds"`
	MaxTranscriptChars    int    `json:"max_transcript_chars"`
	MaxMemoryContentChars int    `json:"max_memory_content_chars"`
	LogFile               string `json:"log_file"`
	// SameProjectBoost is added to scores of memories from the current
	// project; retrieval itself is global (topic-based, not cwd-based).
	SameProjectBoost float64 `json:"same_project_boost"`
	// RulesK caps the always-injected standing-rules section.
	// 0 disables the section entirely; -1 means no limit.
	RulesK int `json:"rules_k"`
	// HookShowRetrieved makes the UserPromptSubmit hook print a compact
	// summary of what it injected (plus the no-match and daemon-down cases)
	// into the Claude Code CLI via the hook's systemMessage field.
	HookShowRetrieved bool `json:"hook_show_retrieved"`
	// HookSummaryLines caps the per-memory lines in that summary; anything
	// beyond it collapses into a "… N more" line. -1 means no limit;
	// 0 falls back to the default.
	HookSummaryLines int `json:"hook_summary_lines"`
	// HookShowSaved makes the Stop hook and the next UserPromptSubmit print
	// what the extractor actually wrote — and why it failed, when it did.
	// Silent save failure is otherwise indistinguishable from a quiet turn.
	HookShowSaved bool `json:"hook_show_saved"`
	// HookSavedLines caps the per-memory lines in that block; anything beyond
	// it collapses into a "… N more" line. -1 means no limit; 0 falls back to
	// the default.
	HookSavedLines int `json:"hook_saved_lines"`
	// HookFlushOnStop makes the Stop hook block on extraction so it can print
	// what THIS turn saved. False falls back to fire-and-forget: the report
	// then prints at the next prompt instead.
	HookFlushOnStop bool `json:"hook_flush_on_stop"`
	// StopFlushBudgetMS caps how long the daemon holds the flush response
	// before answering "running" and finishing in the background.
	StopFlushBudgetMS int `json:"stop_flush_budget_ms"`
	// Backup* control the periodic Cypher dump the daemon writes outside the
	// Docker volume, so losing the volume does not lose the graph.
	BackupEnabled       bool   `json:"backup_enabled"`
	BackupIntervalHours int    `json:"backup_interval_hours"`
	BackupKeep          int    `json:"backup_keep"`
	BackupDir           string `json:"backup_dir"`
}

func Default() Config {
	return Config{
		HTTPAddr:              "127.0.0.1:7690",
		MemgraphURI:           "bolt://127.0.0.1:7687",
		ClaudeBin:             "claude",
		ExtractModel:          "claude-haiku-4-5-20251001",
		ExpandEnabled:         false,
		ExpandModel:           "claude-haiku-4-5-20251001",
		ExpandBudgetMS:        30000,
		RetrieveK:             6,
		RetrieveTimeoutMS:     300,
		DebounceSeconds:       45,
		MaxTranscriptChars:    24000,
		MaxMemoryContentChars: 400,
		LogFile:               "~/.local/state/infinite-memory/imemd.log",
		SameProjectBoost:      1.0,
		RulesK:                50,
		HookShowRetrieved:     true,
		HookSummaryLines:      6,
		HookShowSaved:         true,
		HookSavedLines:        6,
		HookFlushOnStop:       true,
		StopFlushBudgetMS:     90000,
		BackupEnabled:         true,
		BackupIntervalHours:   4,
		BackupKeep:            2,
		BackupDir:             "~/.local/state/infinite-memory/backups",
	}
}

// noLimit is the config value meaning "no cap" for retrieve_k, rules_k and
// hook_summary_lines. Consumers test for <= 0, so any negative works, but
// everything written back to a Config is normalized to this.
const noLimit = -1

// normLimit keeps an explicit no-limit request intact, turns an unset (0)
// field into the compiled default, and passes real caps through.
func normLimit(v, def int) int {
	if v < 0 {
		return noLimit
	}
	if v == 0 {
		return def
	}
	return v
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
	cfg.RetrieveK = normLimit(cfg.RetrieveK, Default().RetrieveK)
	if cfg.DebounceSeconds <= 0 {
		cfg.DebounceSeconds = Default().DebounceSeconds
	}
	if cfg.RulesK < 0 {
		cfg.RulesK = noLimit // any negative means "all of them"; 0 still disables
	}
	cfg.HookSummaryLines = normLimit(cfg.HookSummaryLines, Default().HookSummaryLines)
	cfg.HookSavedLines = normLimit(cfg.HookSavedLines, Default().HookSavedLines)
	if strings.TrimSpace(cfg.ExpandModel) == "" {
		cfg.ExpandModel = Default().ExpandModel
	}
	if cfg.ExpandBudgetMS <= 0 {
		cfg.ExpandBudgetMS = Default().ExpandBudgetMS
	}
	if cfg.StopFlushBudgetMS <= 0 {
		cfg.StopFlushBudgetMS = Default().StopFlushBudgetMS
	}
	if cfg.BackupIntervalHours <= 0 {
		cfg.BackupIntervalHours = Default().BackupIntervalHours
	}
	if cfg.BackupKeep <= 0 {
		cfg.BackupKeep = Default().BackupKeep
	}
	if strings.TrimSpace(cfg.BackupDir) == "" {
		cfg.BackupDir = Default().BackupDir
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

// ExpandBudget caps the query-expansion spawn. Sequential with RetrieveTO,
// never nested inside it: a claude spawn takes seconds, the graph takes
// milliseconds, and one budget cannot serve both.
func (c Config) ExpandBudget() time.Duration {
	if c.ExpandBudgetMS <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.ExpandBudgetMS) * time.Millisecond
}

// SpawnDir is the neutral working directory for extraction claude spawns, so
// they never auto-load a project CLAUDE.md.
func (c Config) SpawnDir() string {
	return ExpandHome("~/.local/state/infinite-memory/spawn")
}

func (c Config) LogPath() string { return ExpandHome(c.LogFile) }

// BackupPath is the host directory holding graph dumps. It deliberately lives
// under ~/.local/state, never inside the Docker volume or the git worktree.
func (c Config) BackupPath() string { return ExpandHome(c.BackupDir) }

func (c Config) BackupInterval() time.Duration {
	if c.BackupIntervalHours <= 0 {
		return 4 * time.Hour
	}
	return time.Duration(c.BackupIntervalHours) * time.Hour
}

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
