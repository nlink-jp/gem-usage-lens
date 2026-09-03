package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/gem-usage-lens/core/budget"
	"github.com/nlink-jp/gem-usage-lens/core/pricing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func isolateHome(t *testing.T) {
	t.Helper()
	// Both homes, so the developer's real config is never picked up.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestLoadMissingIsNotAnError(t *testing.T) {
	isolateHome(t)
	cfg, path, found, err := Load("")
	if err != nil || found || cfg == nil || path == "" {
		t.Fatalf("%v %v %v %q", cfg, found, err, path)
	}
	if !strings.HasSuffix(path, filepath.Join("gem-usage-lens", "config.toml")) {
		t.Fatalf("canonical path: %s", path)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	p := write(t, "[sources]\nsession_root = \"/x\"\n") // typo: session_root
	if _, _, _, err := Load(p); err == nil || !strings.Contains(err.Error(), "session_root") {
		t.Fatalf("unknown key must be an error naming it: %v", err)
	}
}

func TestPricingOverridesInherit(t *testing.T) {
	p := write(t, `
[pricing.models."gemini-3.7-flash"]
input_per_mtok = 1.5

[pricing.models."gemini-9"]
input_per_mtok = 2.0
output_per_mtok = 8.0
cache_read_multiplier = 0.25
`)
	cfg, _, found, err := Load(p)
	if err != nil || !found {
		t.Fatal(err)
	}
	tbl := cfg.PricingTable(pricing.Default())
	r := tbl["gemini-3.7-flash"]
	if r.InputPerMTok != 1.5 || r.OutputPerMTok != 3.75 || r.CacheReadMultiplier != 0.1 || r.GroundingPerReq != 0.014 || r.NonGlobalMultiplier != 1.1 {
		t.Fatalf("partial override must inherit the rest: %+v", r)
	}
	n := tbl["gemini-9"]
	if n.InputPerMTok != 2 || n.OutputPerMTok != 8 || n.CacheReadMultiplier != 0.25 || n.NonGlobalMultiplier != 1.1 || n.GroundingPerReq != 0.014 {
		t.Fatalf("new model must start from the standard modifiers: %+v", n)
	}
	if got := cfg.OverriddenModels(); len(got) != 2 || got[0] != "gemini-3.7-flash" || got[1] != "gemini-9" {
		t.Fatalf("%v", got)
	}
	// The base table is not mutated.
	if pricing.Default()["gemini-3.7-flash"].InputPerMTok != 0.75 {
		t.Fatal("Default mutated")
	}
}

func TestValidateRejectsNegative(t *testing.T) {
	p := write(t, "[pricing.models.\"m\"]\ninput_per_mtok = -1\n")
	if _, _, _, err := Load(p); err == nil {
		t.Fatal("negative rate must fail")
	}
	p = write(t, "[budget]\nmonthly_usd = -5\n")
	if _, _, _, err := Load(p); err == nil {
		t.Fatal("negative budget must fail")
	}
}

func TestBudgetAndSourcesOverrides(t *testing.T) {
	p := write(t, "[sources]\nsessions_root = \"/custom/sessions\"\n[budget]\nmonthly_usd = 120\nwarn_percent = 70\n")
	cfg, _, _, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionsRoot("/default") != "/custom/sessions" {
		t.Fatal("sessions_root")
	}
	lim := cfg.BudgetLimits(budget.DefaultLimits())
	if lim.USD != 120 || lim.WarnPercent != 70 || lim.CriticalPercent != 95 || lim.Tokens != 0 {
		t.Fatalf("%+v", lim)
	}
	var nilCfg *Config
	if nilCfg.SessionsRoot("/d") != "/d" || nilCfg.BudgetLimits(budget.DefaultLimits()).WarnPercent != 80 {
		t.Fatal("nil config must be the defaults")
	}
}

func TestLoadFindsXDGThenDotConfig(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dot := filepath.Join(home, ".config", "gem-usage-lens")
	os.MkdirAll(dot, 0o700)
	os.WriteFile(filepath.Join(dot, "config.toml"), []byte("[budget]\nmonthly_usd = 1\n"), 0o600)
	cfg, path, found, err := Load("")
	if err != nil || !found || *cfg.Budget.MonthlyUSD != 1 || !strings.HasPrefix(path, dot) {
		t.Fatalf("~/.config not found: %v %v %s", err, found, path)
	}
	// XDG wins when both exist.
	xd := filepath.Join(xdg, "gem-usage-lens")
	os.MkdirAll(xd, 0o700)
	os.WriteFile(filepath.Join(xd, "config.toml"), []byte("[budget]\nmonthly_usd = 2\n"), 0o600)
	cfg, path, _, _ = Load("")
	if *cfg.Budget.MonthlyUSD != 2 || !strings.HasPrefix(path, xd) {
		t.Fatalf("XDG must win: %s", path)
	}
}
