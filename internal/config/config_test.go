package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

// hook_saved_lines follows the same -1 / 0 / n convention as its retrieve twin.
func TestLoadSavedLinesConvention(t *testing.T) {
	if got := loadWith(t, `{"hook_saved_lines":-1}`).HookSavedLines; got != noLimit {
		t.Fatalf("-1 must mean no limit, got %d", got)
	}
	if got := loadWith(t, `{"hook_saved_lines":-9}`).HookSavedLines; got != noLimit {
		t.Fatalf("any negative normalizes to noLimit, got %d", got)
	}
	if got := loadWith(t, `{"hook_saved_lines":0}`).HookSavedLines; got != Default().HookSavedLines {
		t.Fatalf("0 must fall back to the default, got %d", got)
	}
	if got := loadWith(t, `{"hook_saved_lines":3}`).HookSavedLines; got != 3 {
		t.Fatalf("a real cap must survive, got %d", got)
	}
}

// Both new bools default to true, so an explicit false has to win.
func TestLoadSaveBoolsCanBeDisabled(t *testing.T) {
	cfg := loadWith(t, `{"hook_show_saved":false,"hook_flush_on_stop":false}`)
	if cfg.HookShowSaved {
		t.Fatal("hook_show_saved:false must survive Load")
	}
	if cfg.HookFlushOnStop {
		t.Fatal("hook_flush_on_stop:false must survive Load")
	}
	def := loadWith(t, `{}`)
	if !def.HookShowSaved || !def.HookFlushOnStop {
		t.Fatal("both default to true when absent")
	}
}

func TestLoadFlushBudgetFallsBack(t *testing.T) {
	if got := loadWith(t, `{"stop_flush_budget_ms":0}`).StopFlushBudgetMS; got != Default().StopFlushBudgetMS {
		t.Fatalf("0 must fall back to the default, got %d", got)
	}
	if got := loadWith(t, `{"stop_flush_budget_ms":-5}`).StopFlushBudgetMS; got != Default().StopFlushBudgetMS {
		t.Fatalf("negative is meaningless here, want default, got %d", got)
	}
	if got := loadWith(t, `{"stop_flush_budget_ms":15000}`).StopFlushBudgetMS; got != 15000 {
		t.Fatalf("a real budget must survive, got %d", got)
	}
}

// expand_enabled inverts this file's usual bool polarity: everything else
// defaults true and relies on "explicit false wins", this one defaults false
// and relies on "explicit true wins".
func TestExpandEnabledPolarity(t *testing.T) {
	if Default().ExpandEnabled {
		t.Fatal("expansion must be off unless asked for: it adds seconds to every prompt")
	}
	if got := loadWith(t, `{}`); got.ExpandEnabled {
		t.Fatal("empty config must leave expansion off")
	}
	if got := loadWith(t, `{"expand_enabled": true}`); !got.ExpandEnabled {
		t.Fatal("explicit true must win")
	}
}

func TestExpandBudgetFallsBackToDefault(t *testing.T) {
	for _, body := range []string{`{}`, `{"expand_budget_ms": 0}`, `{"expand_budget_ms": -5}`} {
		if got := loadWith(t, body).ExpandBudget(); got != 30*time.Second {
			t.Fatalf("%s -> %v, want the compiled default", body, got)
		}
	}
	if got := loadWith(t, `{"expand_budget_ms": 4000}`).ExpandBudget(); got != 4*time.Second {
		t.Fatalf("got %v, want 4s", got)
	}
}

// Sharing extract_model would drag a large, slow model onto the retrieval path.
func TestExpandModelIsIndependent(t *testing.T) {
	got := loadWith(t, `{"extract_model": "claude-opus-5"}`)
	if got.ExpandModel == got.ExtractModel {
		t.Fatal("expand_model must not follow extract_model")
	}
	if got := loadWith(t, `{"expand_model": ""}`).ExpandModel; got != Default().ExpandModel {
		t.Fatalf("blank expand_model -> %q, want the default", got)
	}
}
