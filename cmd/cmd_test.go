package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/aggregate"
	"github.com/nlink-jp/gem-usage-lens/core/budget"
	"github.com/nlink-jp/gem-usage-lens/core/config"
	"github.com/nlink-jp/gem-usage-lens/core/pricing"
)

// `--version` and `version` must print the same line: the Homebrew formula
// test greps the flag form.
func TestVersionFlagAndSubcommandAgree(t *testing.T) {
	var a, b bytes.Buffer
	if code := run([]string{"--version"}, "v1.2.3", &a, &a); code != 0 {
		t.Fatalf("--version exit %d", code)
	}
	if code := run([]string{"version"}, "v1.2.3", &b, &b); code != 0 {
		t.Fatalf("version exit %d", code)
	}
	if a.String() != b.String() || a.String() != "gem-usage-lens v1.2.3\n" {
		t.Fatalf("%q vs %q", a.String(), b.String())
	}
}

func TestUnknownCommandExits2(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"bogus"}, "v", &out, &out); code != 2 || !strings.Contains(out.String(), "unknown command") {
		t.Fatalf("%d %s", code, out.String())
	}
	if code := run(nil, "v", &out, &out); code != 2 {
		t.Fatal("no args must show usage and exit 2")
	}
}

func TestParseSinceForms(t *testing.T) {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, tokyo)
	cases := map[string]time.Time{
		"":                     {},
		"today":                time.Date(2026, 9, 15, 0, 0, 0, 0, tokyo),
		"month":                time.Date(2026, 9, 1, 0, 0, 0, 0, tokyo),
		"7d":                   now.AddDate(0, 0, -7),
		"2026-09-10":           time.Date(2026, 9, 10, 0, 0, 0, 0, tokyo),
		"2026-09-10T09:30":     time.Date(2026, 9, 10, 9, 30, 0, 0, tokyo),
		"2026-09-10T00:00:00Z": time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := parseSince(in, tokyo, now)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if want.IsZero() && got != 0 {
			t.Fatalf("%q: want 0 got %d", in, got)
		}
		if !want.IsZero() && got != want.Unix() {
			t.Fatalf("%q: got %v want %v", in, time.Unix(got, 0).In(tokyo), want)
		}
	}
	if _, err := parseSince("yesterday", tokyo, now); err == nil {
		t.Fatal("bad form must error")
	}
	// A bare --until date is inclusive.
	u, _ := parseUntil("2026-09-10", tokyo, now)
	if u != time.Date(2026, 9, 10, 23, 59, 59, 0, tokyo).Unix() {
		t.Fatalf("until: %v", time.Unix(u, 0).In(tokyo))
	}
}

func TestResolveRootPrecedence(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sources.SessionsRoot = "/from/config"
	if resolveRoot("/default", cfg, "") != "/from/config" {
		t.Fatal("config over default")
	}
	if resolveRoot("/default", cfg, " /from/flag ") != "/from/flag" {
		t.Fatal("flag over config")
	}
	if resolveRoot("/default", &config.Config{}, "") != "/default" {
		t.Fatal("default")
	}
}

func TestBudgetLines(t *testing.T) {
	w := budget.Window{Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	mid := w.Start.Add(15 * 24 * time.Hour)
	lim := budget.Limits{USD: 200, WarnPercent: 80, CriticalPercent: 95}

	s := budget.Build(lim, budget.Consumption{USD: 150}, w, mid, 0)
	if got := basisLine(s.Cost, moneyFmt); got != "$150.00 used / $200.00 limit   75% used · $50.00 left (25%)   [normal]" {
		t.Fatalf("%q", got)
	}
	if got := forecastLine(s.Cost, moneyFmt, time.UTC); !strings.HasPrefix(got, "pace: on pace for $300.00 (150%) — budget gone Sep 21 ") {
		t.Fatalf("%q", got)
	}
	s = budget.Build(lim, budget.Consumption{USD: 50}, w, mid, 0)
	if got := forecastLine(s.Cost, moneyFmt, time.UTC); got != "pace: on pace for $100.00 (50%) by the reset" {
		t.Fatalf("%q", got)
	}
	s = budget.Build(lim, budget.Consumption{USD: 250}, w, mid, 0)
	if got := forecastLine(s.Cost, moneyFmt, time.UTC); got != "pace: over budget — on pace for $500.00 (250%)" {
		t.Fatalf("%q", got)
	}
	s = budget.Build(lim, budget.Consumption{USD: 20}, w, w.Start.Add(time.Hour), 0)
	if got := forecastLine(s.Cost, moneyFmt, time.UTC); got != "pace: too early this month to project" {
		t.Fatalf("%q", got)
	}
	// Unset basis: used only, and how to set a limit.
	if got := basisLine(s.Tokens, tokenFmt); !strings.HasPrefix(got, "0 used (no limit set") {
		t.Fatalf("%q", got)
	}
	if forecastLine(s.Tokens, tokenFmt, time.UTC) != "" {
		t.Fatal("unset basis has no pace line")
	}
	if tokenFmt(1_234_567) != "1.2M" || tokenFmt(12_345) != "12.3k" || tokenFmt(12) != "12" {
		t.Fatal("tokenFmt")
	}
}

// The whole budget text must render every state without going quiet.
func TestPrintBudgetMentionsEveryLine(t *testing.T) {
	w := budget.Window{Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	s := budget.Build(budget.Limits{USD: 100, Tokens: 1_000_000, WarnPercent: 80, CriticalPercent: 95},
		budget.Consumption{USD: 90, Tokens: 100_000}, w, w.Start.Add(10*24*time.Hour), 2)
	var out bytes.Buffer
	printBudget(&out, s, time.UTC)
	for _, want := range []string{"month:   2026-09-01 → 2026-10-01", "cost:    $90.00 used", "[warning]", "tokens:  100.0k used / 1.0M limit", "partial: 2 record(s)", "pace:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
}

// The sessions listing is chronological by default: since gem-agent ADR-0071
// a session id is a UUID, so the old key order is no longer time order.
func TestSessionsDefaultSortIsTime(t *testing.T) {
	if sessionsDefaultSort != "time" {
		t.Fatalf("sessions default sort %q", sessionsDefaultSort)
	}
}

func TestPrintReportTimeColumns(t *testing.T) {
	rows := []aggregate.Row{
		{Key: "2ebb723d-0712-4fcc-bf4d-7745d637e70a", Records: 2, TotalTokens: 10, CostUSD: 0.5, FirstRecord: "2026-09-05T03:50:12+09:00", LastRecord: "2026-09-05T04:10:00+09:00"},
		{Key: "unknown", Records: 1},
	}
	var without, with bytes.Buffer
	printReport(&without, rows, false)
	printReport(&with, rows, true)
	if strings.Contains(without.String(), "STARTED") {
		t.Fatalf("report must not grow time columns:\n%s", without.String())
	}
	out := with.String()
	for _, want := range []string{"STARTED", "LAST", "2026-09-05 03:50", "2026-09-05 04:10"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	// The bounded row carries both stamps, the unbounded one two dashes,
	// and TOTAL leaves the time cells empty while keeping its numbers.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if n := len(strings.Fields(lines[1])); n != 13 { // key, 2×(date time), 8 numbers
		t.Fatalf("%d fields in %q", n, lines[1])
	}
	if !strings.Contains(lines[2], "—") || strings.Count(lines[2], "—") != 2 {
		t.Fatalf("unbounded row: %q", lines[2])
	}
	if !strings.HasPrefix(lines[3], "TOTAL") || !strings.HasSuffix(lines[3], "$0.5000") {
		t.Fatalf("total row: %q", lines[3])
	}
	if tableTime("") != "—" || tableTime("garbage") != "garbage" {
		t.Fatal("tableTime fallbacks")
	}
}

// `models --json` keeps "models" in its original shape — model → the rates in
// force now, the same fields — and adds "schedule" with every period, "from"
// always written ("" for the open first period). Pinned through a JSON round
// trip, since the bytes are the contract.
func TestModelsJSONShape(t *testing.T) {
	boundary := time.Date(2027, 1, 1, 0, 0, 0, 0, time.FixedZone("PST", -8*3600))
	tbl := pricing.Table{
		"flat": pricing.Flat(pricing.StandardRates(1, 5)),
		"sched": {
			{Rates: pricing.StandardRates(0.75, 3.75)},
			{From: boundary, Rates: pricing.StandardRates(1.50, 7.50)},
		},
	}
	decode := func(now time.Time) map[string]any {
		b, err := json.Marshal(modelsJSON(tbl, now))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}

	before := decode(boundary.Add(-time.Second))
	if before["verified_on"] != pricing.VerifiedOn {
		t.Fatalf("verified_on: %v", before["verified_on"])
	}
	models := before["models"].(map[string]any)
	sched := models["sched"].(map[string]any)
	for _, k := range []string{"input_per_mtok", "output_per_mtok", "cache_read_multiplier", "grounding_per_req", "non_global_multiplier"} {
		if _, ok := sched[k]; !ok {
			t.Fatalf("models entry lost %q: %v", k, sched)
		}
	}
	if _, ok := sched["from"]; ok {
		t.Fatal(`"models" must keep its original fields; "from" belongs to "schedule"`)
	}
	if sched["input_per_mtok"] != 0.75 {
		t.Fatalf("in force before the boundary: %v", sched["input_per_mtok"])
	}
	after := decode(boundary)
	if got := after["models"].(map[string]any)["sched"].(map[string]any)["input_per_mtok"]; got != 1.5 {
		t.Fatalf("in force from the boundary: %v", got)
	}

	periods := before["schedule"].(map[string]any)["sched"].([]any)
	if len(periods) != 2 {
		t.Fatalf("schedule: %v", periods)
	}
	first, second := periods[0].(map[string]any), periods[1].(map[string]any)
	if v, ok := first["from"]; !ok || v != "" {
		t.Fatalf(`the open period must carry "from": "" — got %v (present=%v)`, v, ok)
	}
	if second["from"] != "2027-01-01T00:00:00-08:00" || second["input_per_mtok"] != 1.5 {
		t.Fatalf("second period: %v", second)
	}
	if flat := before["schedule"].(map[string]any)["flat"].([]any); len(flat) != 1 {
		t.Fatalf("a flat model has one period: %v", flat)
	}
}

func TestPrintModelsShowsEveryPeriod(t *testing.T) {
	boundary := time.Date(2027, 1, 1, 0, 0, 0, 0, time.FixedZone("PST", -8*3600))
	tbl := pricing.Table{
		"sched": {
			{Rates: pricing.StandardRates(0.75, 3.75)},
			{From: boundary, Rates: pricing.StandardRates(1.50, 7.50)},
		},
	}
	var buf bytes.Buffer
	printModels(&buf, tbl, &config.Config{}, boundary.Add(time.Hour))
	out := buf.String()
	for _, want := range []string{"FROM", "sched  —", "*2027-01-01T00:00:00-08:00", "$1.50", "* = in force now."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Exactly at the boundary the new period is the one in force; a second
	// before, the first one is.
	for _, c := range []struct {
		now  time.Time
		want string
	}{
		{boundary, "*2027-01-01T00:00:00-08:00"},
		{boundary.Add(-time.Second), "sched  *—"},
	} {
		buf.Reset()
		printModels(&buf, tbl, &config.Config{}, c.now)
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("at %v: missing %q in:\n%s", c.now, c.want, buf.String())
		}
	}
	// A table with no price change stars nothing.
	buf.Reset()
	printModels(&buf, pricing.Table{"flat": pricing.Flat(pricing.StandardRates(1, 5))}, &config.Config{}, boundary)
	if strings.Contains(buf.String(), "*") {
		t.Errorf("no star without a price change:\n%s", buf.String())
	}
}
