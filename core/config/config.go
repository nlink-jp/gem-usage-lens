// Package config loads gem-usage-lens's optional TOML configuration.
//
// Every setting is optional and a missing file is not an error — an
// unconfigured install runs entirely on the inferred defaults. Precedence,
// highest first:
//
//	CLI flags  >  config file  >  built-in / inferred defaults
//
// The file is searched at the paths platform.ConfigSearchPaths lists. Decoding
// is strict: an unrecognised key is a hard error rather than a silently
// ignored one, because a typo'd setting that appears to work is the worst
// outcome for a file whose whole job is to override behaviour.
//
// Merging is expressed as pure functions over explicit inputs, so the
// resolution rules are unit-testable without touching the filesystem.
package config

import (
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/nlink-jp/gem-usage-lens/core/budget"
	"github.com/nlink-jp/gem-usage-lens/core/platform"
	"github.com/nlink-jp/gem-usage-lens/core/pricing"
)

// Config is the root of config.toml. A zero Config is valid and changes nothing.
type Config struct {
	Sources Sources `toml:"sources"`
	Pricing Pricing `toml:"pricing"`
	Budget  Budget  `toml:"budget"`
}

// Sources overrides the inferred sessions root — the safety valve for a
// gem-agent state directory that lives somewhere unusual.
type Sources struct {
	SessionsRoot string `toml:"sessions_root"`
}

// Pricing overrides or extends the built-in rate table, keyed by model id
// (`[pricing.models."gemini-3.7-flash"]`).
type Pricing struct {
	Models map[string]RateOverride `toml:"models"`
}

// RateOverride is a *partial* rate entry: every field is optional, and an
// omitted field inherits from the built-in entry (or, for a model the table
// does not know, from the standard modifiers).
//
// The fields are pointers precisely so "omitted" and "explicitly 0" are
// distinguishable. With plain floats, overriding just input_per_mtok would
// silently zero the cache multiplier — the silent-undercount failure mode
// this table exists to prevent.
type RateOverride struct {
	InputPerMTok        *float64 `toml:"input_per_mtok"`
	OutputPerMTok       *float64 `toml:"output_per_mtok"`
	CacheReadMultiplier *float64 `toml:"cache_read_multiplier"`
	GroundingPerReq     *float64 `toml:"grounding_per_req"`
	NonGlobalMultiplier *float64 `toml:"non_global_multiplier"`
}

// Budget holds the calendar-month budget defaults `budget` uses when the
// flags are not given. Pointers so an unset field keeps the built-in default.
type Budget struct {
	MonthlyUSD      *float64 `toml:"monthly_usd"`
	MonthlyTokens   *float64 `toml:"monthly_tokens"`
	WarnPercent     *float64 `toml:"warn_percent"`
	CriticalPercent *float64 `toml:"critical_percent"`
}

// Load reads the config at path, or at the first existing search path when
// path is empty. It returns the resolved path and whether a file was actually
// found, so callers (notably `doctor`) can tell "using defaults" from "loaded
// overrides".
//
// A missing file yields a zero Config and found=false, not an error. A file
// that exists but cannot be parsed — or that contains an unknown key — IS an
// error: silently ignoring it would leave the user believing a setting took
// effect.
func Load(path string) (cfg *Config, resolvedPath string, found bool, err error) {
	cfg = &Config{}
	if path == "" {
		path, found, err = platform.ConfigFilePath()
		if err != nil {
			return cfg, "", false, err
		}
		if !found {
			return cfg, path, false, nil
		}
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return cfg, path, false, nil
		}
		return cfg, path, false, statErr
	}

	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return cfg, path, true, fmt.Errorf("parse config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		return cfg, path, true, fmt.Errorf(
			"unknown key(s) in %s: %s — see config.example.toml for the accepted schema",
			path, strings.Join(keys, ", "))
	}
	if err := cfg.Validate(); err != nil {
		return cfg, path, true, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, path, true, nil
}

// Validate rejects values that would silently corrupt cost figures.
func (c *Config) Validate() error {
	for name, ov := range c.Pricing.Models {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("[pricing.models] has an empty model id")
		}
		for _, f := range []struct {
			key string
			val *float64
		}{
			{"input_per_mtok", ov.InputPerMTok},
			{"output_per_mtok", ov.OutputPerMTok},
			{"cache_read_multiplier", ov.CacheReadMultiplier},
			{"grounding_per_req", ov.GroundingPerReq},
			{"non_global_multiplier", ov.NonGlobalMultiplier},
		} {
			if f.val != nil && *f.val < 0 {
				return fmt.Errorf("[pricing.models.%q] %s is negative (%v)", name, f.key, *f.val)
			}
		}
	}
	for _, f := range []struct {
		key string
		val *float64
	}{
		{"monthly_usd", c.Budget.MonthlyUSD},
		{"monthly_tokens", c.Budget.MonthlyTokens},
		{"warn_percent", c.Budget.WarnPercent},
		{"critical_percent", c.Budget.CriticalPercent},
	} {
		if f.val != nil && *f.val < 0 {
			return fmt.Errorf("[budget] %s is negative (%v)", f.key, *f.val)
		}
	}
	return nil
}

// SessionsRoot applies the [sources] override on top of the inferred default.
func (c *Config) SessionsRoot(def string) string {
	if c == nil {
		return def
	}
	if v := strings.TrimSpace(c.Sources.SessionsRoot); v != "" {
		return v
	}
	return def
}

// PricingTable applies the [pricing.models] overrides on top of base,
// returning a new table (base is not mutated). A model absent from base is
// added, starting from the standard modifiers, so a two-line override is
// enough to price a model this build has never heard of.
func (c *Config) PricingTable(base pricing.Table) pricing.Table {
	out := make(pricing.Table, len(base))
	maps.Copy(out, base)
	if c == nil {
		return out
	}
	for name, ov := range c.Pricing.Models {
		r, known := out[name]
		if !known {
			r = pricing.StandardRates(0, 0)
		}
		setIf(&r.InputPerMTok, ov.InputPerMTok)
		setIf(&r.OutputPerMTok, ov.OutputPerMTok)
		setIf(&r.CacheReadMultiplier, ov.CacheReadMultiplier)
		setIf(&r.GroundingPerReq, ov.GroundingPerReq)
		setIf(&r.NonGlobalMultiplier, ov.NonGlobalMultiplier)
		out[name] = r
	}
	return out
}

// BudgetLimits applies the [budget] section on top of def.
func (c *Config) BudgetLimits(def budget.Limits) budget.Limits {
	if c == nil {
		return def
	}
	setIf(&def.USD, c.Budget.MonthlyUSD)
	setIf(&def.Tokens, c.Budget.MonthlyTokens)
	setIf(&def.WarnPercent, c.Budget.WarnPercent)
	setIf(&def.CriticalPercent, c.Budget.CriticalPercent)
	return def
}

// OverriddenModels lists the model ids the config touches, sorted — used to
// tell the user which prices are theirs rather than the built-in ones.
func (c *Config) OverriddenModels() []string {
	if c == nil || len(c.Pricing.Models) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Pricing.Models))
	for name := range c.Pricing.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func setIf(dst *float64, src *float64) {
	if src != nil {
		*dst = *src
	}
}
