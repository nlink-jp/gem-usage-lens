package cost

import (
	"math"
	"testing"
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/model"
	"github.com/nlink-jp/gem-usage-lens/core/pricing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The four accounting rules, pinned one at a time against gemini-3.7-flash's
// built-in rates ($0.75 in / $3.75 out / cache 0.1× / grounding $0.014 /
// non-global 1.1×).
func TestComputeRules(t *testing.T) {
	r, ok := pricing.Default().Lookup("gemini-3.7-flash", time.Time{})
	if !ok {
		t.Fatal("gemini-3.7-flash must be in the built-in table")
	}

	// 1M uncached prompt tokens = $0.75.
	if got := Compute(model.Usage{Prompt: 1_000_000, Total: 1_000_000}, r, model.SourceMain, "global"); !near(got, 0.75) {
		t.Fatalf("uncached prompt: %v", got)
	}
	// Thoughts bill at the output price: 1M output + 1M thoughts = 2 × $3.75.
	if got := Compute(model.Usage{Output: 1_000_000, Thoughts: 1_000_000, Total: 2_000_000}, r, model.SourceMain, "global"); !near(got, 7.5) {
		t.Fatalf("thoughts as output: %v", got)
	}
	// Cached is a share of prompt: 1M prompt all cached = $0.075, not $0.75 + $0.075.
	if got := Compute(model.Usage{Prompt: 1_000_000, Cached: 1_000_000, Total: 1_000_000}, r, model.SourceMain, "global"); !near(got, 0.075) {
		t.Fatalf("cached share: %v", got)
	}
	// Half cached: 500k × $0.75 + 500k × $0.075.
	if got := Compute(model.Usage{Prompt: 1_000_000, Cached: 500_000, Total: 1_000_000}, r, model.SourceMain, "global"); !near(got, 0.375+0.0375) {
		t.Fatalf("half cached: %v", got)
	}
	// Grounding: a web_search call adds $0.014 on top of its tokens.
	base := Compute(model.Usage{Prompt: 47, Output: 617, Total: 664}, r, model.SourceMain, "global")
	if got := Compute(model.Usage{Prompt: 47, Output: 617, Total: 664}, r, model.SourceWebSearch, "global"); !near(got, base+0.014) {
		t.Fatalf("grounding: %v vs base %v", got, base)
	}
	// Tool results fed back as input bill at the input price, never cached.
	if got := Compute(model.Usage{Prompt: 1000, ToolPrompt: 1_000_000, Total: 1_001_000}, r, model.SourceWebFetch, "global"); !near(got, 0.75+1000*0.75/1e6) {
		t.Fatalf("tool prompt: %v", got)
	}
	// Region: a non-global session bills 1.1× the whole amount; "" counts as global.
	if got := Compute(model.Usage{Prompt: 1_000_000, Total: 1_000_000}, r, model.SourceMain, "us-central1"); !near(got, 0.825) {
		t.Fatalf("non-global: %v", got)
	}
	if got := Compute(model.Usage{Prompt: 1_000_000, Total: 1_000_000}, r, model.SourceMain, ""); !near(got, 0.75) {
		t.Fatalf("empty location must be global: %v", got)
	}
}

// A real round from a transcript: 57,068 prompt (40,000 cached), 49 output,
// 57 thoughts on gemini-3.7-flash, worked out by hand.
func TestComputeRealRound(t *testing.T) {
	r, _ := pricing.Default().Lookup("gemini-3.7-flash", time.Time{})
	u := model.Usage{Prompt: 57068, Output: 49, Thoughts: 57, Cached: 40000, Total: 57174}
	want := (17068*0.75 + 40000*0.075 + 106*3.75) / 1e6
	if got := Compute(u, r, model.SourceMain, "global"); !near(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestComputeClampsCachedAbovePrompt(t *testing.T) {
	r, _ := pricing.Default().Lookup("gemini-3.7-flash", time.Time{})
	got := Compute(model.Usage{Prompt: 100, Cached: 200}, r, model.SourceMain, "global")
	if got < 0 || !near(got, 200*0.075/1e6) {
		t.Fatalf("cached above prompt must not go negative: %v", got)
	}
}

func TestComputeRecordUnknownModelIsZero(t *testing.T) {
	rec := model.UsageRecord{Model: "gemini-99", Usage: model.Usage{Prompt: 1000, Total: 1000}}
	if c := ComputeRecord(rec, pricing.Default()); c.ListPriceUSD != 0 {
		t.Fatalf("unknown model must cost 0 (and be surfaced as unpriced), got %v", c)
	}
	known := model.UsageRecord{Model: "gemini-3.5-flash-lite", Source: model.SourceRisk, Location: "global",
		Usage: model.Usage{Prompt: 1_000_000, Total: 1_000_000}}
	if c := ComputeRecord(known, pricing.Default()); !near(c.ListPriceUSD, 0.30) {
		t.Fatalf("flash-lite prompt: %v", c)
	}
}

// The cost engine must use the record's own model's multiplier, not a
// constant: a config-priced model with a different cache multiplier bills
// cache hits differently.
func TestComputeUsesPerModelCacheMultiplier(t *testing.T) {
	r := pricing.StandardRates(1, 4)
	r.CacheReadMultiplier = 0.25
	u := model.Usage{Prompt: 1_000_000, Cached: 1_000_000, Total: 1_000_000}
	if got := Compute(u, r, model.SourceMain, "global"); !near(got, 0.25) {
		t.Fatalf("per-model multiplier ignored: %v", got)
	}
}

// ComputeRecord prices a record at the rate in force at its own timestamp
// (ADR-0001), on either side of a price change.
func TestComputeRecordUsesTheRecordsTime(t *testing.T) {
	boundary := time.Date(2027, 1, 1, 8, 0, 0, 0, time.UTC) // 2027-01-01T00:00-08:00
	tbl := pricing.Table{"m": {
		{Rates: pricing.StandardRates(0.75, 3.75)},
		{From: boundary, Rates: pricing.StandardRates(1.50, 7.50)},
	}}
	u := model.Usage{Prompt: 1_000_000, Total: 1_000_000}
	for _, c := range []struct {
		at   time.Time
		want float64
	}{
		{boundary.Add(-time.Second), 0.75},
		{boundary, 1.50},
		{time.Time{}, 0.75}, // no timestamp → first period
	} {
		rec := model.UsageRecord{Model: "m", Timestamp: c.at, Location: "global", Usage: u}
		if got := ComputeRecord(rec, tbl).ListPriceUSD; !near(got, c.want) {
			t.Errorf("at %v: %v, want %v", c.at, got, c.want)
		}
	}
}
