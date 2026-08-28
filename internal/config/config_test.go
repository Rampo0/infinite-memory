package config

import (
	"os"
	"path/filepath"
	"testing"
)

func loadWith(t *testing.T, body string) Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("IMEM_CONFIG", p)
	return Load()
}

// -1 survives Load for every capped knob: the retriever, not the config
// loader, is what decides "no limit" means fetch everything.
func TestLoadKeepsNoLimit(t *testing.T) {
	cfg := loadWith(t, `{"retrieve_k":-1,"rules_k":-1,"hook_summary_lines":-1}`)
	if cfg.RetrieveK != -1 {
		t.Fatalf("retrieve_k: want -1, got %d", cfg.RetrieveK)
	}
	if cfg.RulesK != -1 {
		t.Fatalf("rules_k: want -1, got %d", cfg.RulesK)
	}
	if cfg.HookSummaryLines != -1 {
		t.Fatalf("hook_summary_lines: want -1, got %d", cfg.HookSummaryLines)
	}
}

// Any negative normalizes to -1 so consumers only ever see the one sentinel.
func TestLoadNormalizesNegatives(t *testing.T) {
	cfg := loadWith(t, `{"retrieve_k":-99,"rules_k":-7,"hook_summary_lines":-2}`)
	if cfg.RetrieveK != -1 || cfg.RulesK != -1 || cfg.HookSummaryLines != -1 {
		t.Fatalf("want all -1, got k=%d rules=%d lines=%d",
			cfg.RetrieveK, cfg.RulesK, cfg.HookSummaryLines)
	}
}

// 0 is "unset" for retrieve_k and hook_summary_lines, but "disabled" for
// rules_k — the standing-rules section is the only one you can turn off.
func TestLoadZeroSemantics(t *testing.T) {
	cfg := loadWith(t, `{"retrieve_k":0,"rules_k":0,"hook_summary_lines":0}`)
	if cfg.RetrieveK != Default().RetrieveK {
		t.Fatalf("retrieve_k 0 must fall back to default, got %d", cfg.RetrieveK)
	}
	if cfg.HookSummaryLines != Default().HookSummaryLines {
		t.Fatalf("hook_summary_lines 0 must fall back to default, got %d", cfg.HookSummaryLines)
	}
	if cfg.RulesK != 0 {
		t.Fatalf("rules_k 0 must stay 0 (disabled), got %d", cfg.RulesK)
	}
}

func TestLoadKeepsRealCaps(t *testing.T) {
	cfg := loadWith(t, `{"retrieve_k":20,"rules_k":50,"hook_summary_lines":8}`)
	if cfg.RetrieveK != 20 || cfg.RulesK != 50 || cfg.HookSummaryLines != 8 {
		t.Fatalf("caps mangled: k=%d rules=%d lines=%d",
			cfg.RetrieveK, cfg.RulesK, cfg.HookSummaryLines)
	}
}

// A missing file must never fail: hooks work with zero setup.
func TestLoadMissingFileUsesDefaults(t *testing.T) {
	t.Setenv("IMEM_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	if got, want := Load().RetrieveK, Default().RetrieveK; got != want {
		t.Fatalf("want default %d, got %d", want, got)
	}
}
