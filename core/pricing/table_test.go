package pricing

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultTableAgainstPricingPage(t *testing.T) {
	// The Gemini 3 rows of the Vertex AI pricing page, global endpoint, as
	// verified on VerifiedOn. If a sync changes one of these, change the
	// expectation deliberately and bump VerifiedOn.
	want := map[string][2]float64{
		"gemini-3.8-flash":       {0.75, 3.75},
		"gemini-3.7-flash":       {0.75, 3.75},
		"gemini-3.6-flash":       {0.75, 3.75},
		"gemini-3.5-flash":       {1.50, 9.00},
		"gemini-3.5-flash-lite":  {0.30, 2.50},
		"gemini-3.1-flash-lite":  {0.25, 1.50},
		"gemini-3-flash-preview": {0.50, 3.00},
	}
	tbl := Default()
	if len(tbl) != len(want) {
		t.Fatalf("table has %d models, test knows %d — keep them in step", len(tbl), len(want))
	}
	for m, io := range want {
		ps, ok := tbl[m]
		if !ok {
			t.Fatalf("%s missing", m)
		}
		if len(ps) != 1 {
			t.Fatalf("%s: %d periods — a price change is appended only once it has happened, and this test must then pin every period", m, len(ps))
		}
		r := ps[0].Rates
		if r.InputPerMTok != io[0] || r.OutputPerMTok != io[1] {
			t.Fatalf("%s: %v/%v want %v/%v", m, r.InputPerMTok, r.OutputPerMTok, io[0], io[1])
		}
		// Every Gemini 3 row on the page prices cached input at 0.1× input and
		// non-global at 1.1×; grounding is $14 per 1,000 Grounding Queries
		// (the Gemini 3 row — the $35 row on the page belongs to Gemini 2.x).
		if r.CacheReadMultiplier != 0.1 || r.NonGlobalMultiplier != 1.1 || r.GroundingPerReq != 0.014 {
			t.Fatalf("%s modifiers: %+v", m, r)
		}
	}
	if VerifiedOn == "" {
		t.Fatal("VerifiedOn must name the day the table was checked")
	}
	if err := tbl.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The table states only prices in force (ADR-0001 §3): a scheduled price is
// not written ahead of time, so no built-in period may start after the day the
// table was verified.
func TestDefaultTableHoldsNoScheduledPrice(t *testing.T) {
	verified, err := time.Parse("2006-01-02", VerifiedOn)
	if err != nil {
		t.Fatalf("VerifiedOn %q is not a date: %v", VerifiedOn, err)
	}
	endOfDay := verified.AddDate(0, 0, 1)
	for m, ps := range Default() {
		for _, p := range ps {
			if !p.From.Before(endOfDay) {
				t.Errorf("%s: period from %s starts after VerifiedOn %s — write it once it has happened", m, p.From.Format(time.RFC3339), VerifiedOn)
			}
		}
	}
}

func instant(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A synthetic model with two price changes, one on a PST midnight and one on
// a PDT midnight — the offsets differ, which is why each start is written as
// a literal instant rather than derived from a zone.
func scheduled(t *testing.T) Table {
	return Table{"m": {
		{Rates: rates(0.75, 3.75)},
		{From: instant(t, "2027-01-01T00:00:00-08:00"), Rates: rates(1.50, 7.50)},
		{From: instant(t, "2027-07-01T00:00:00-07:00"), Rates: rates(2.00, 9.00)},
	}}
}

func TestLookupPicksThePeriodInForce(t *testing.T) {
	tbl := scheduled(t)
	if err := tbl.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		at   string
		want float64
	}{
		{"2026-12-31T23:59:59-08:00", 0.75},
		{"2027-01-01T07:59:59Z", 0.75},
		{"2027-01-01T07:59:59.999999999Z", 0.75},
		{"2027-01-01T08:00:00Z", 1.50}, // exactly at the start: the new price
		{"2027-01-01T08:00:01Z", 1.50},
		{"2027-01-01T17:00:00+09:00", 1.50}, // 17:00 JST is the same instant
		{"2027-07-01T06:59:59Z", 1.50},
		{"2027-07-01T07:00:00Z", 2.00}, // PDT midnight is 07:00Z, not 08:00Z
		{"2030-01-01T00:00:00Z", 2.00},
	}
	for _, c := range cases {
		r, ok := tbl.Lookup("m", instant(t, c.at))
		if !ok || r.InputPerMTok != c.want {
			t.Errorf("at %s: got %v (ok=%v), want %v", c.at, r.InputPerMTok, ok, c.want)
		}
	}
	// A record without a timestamp — zero time here, 1970 once stored — falls
	// in the first period, so repricing it never depends on the clock.
	for _, at := range []time.Time{{}, time.Unix(0, 0)} {
		if r, _ := tbl.Lookup("m", at); r.InputPerMTok != 0.75 {
			t.Errorf("at %v: got %v, want the first period", at, r.InputPerMTok)
		}
	}
	// The snapshot-suffix normalisation reaches the same periods.
	if r, ok := tbl.Lookup("m-001", instant(t, "2027-02-01T00:00:00Z")); !ok || r.InputPerMTok != 1.50 {
		t.Errorf("m-001: %v ok=%v", r.InputPerMTok, ok)
	}
	if got := tbl.Current(instant(t, "2027-02-01T00:00:00Z"))["m"].InputPerMTok; got != 1.50 {
		t.Errorf("Current: %v", got)
	}
}

func TestKnownIgnoresTime(t *testing.T) {
	tbl := scheduled(t)
	if !tbl.Known("m") || !tbl.Known("m@default") || tbl.Known("other") || tbl.Known("") {
		t.Fatal("Known must answer by model id alone")
	}
	if (Table{"empty": nil}).Known("empty") {
		t.Fatal("a model with no period is not priced")
	}
}

func TestValidateRejectsBrokenPeriods(t *testing.T) {
	at := instant(t, "2027-01-01T08:00:00Z")
	cases := map[string]Table{
		"no period":        {"m": nil},
		"first not open":   {"m": {{From: at, Rates: rates(1, 1)}}},
		"second not later": {"m": {{Rates: rates(1, 1)}, {From: at, Rates: rates(2, 2)}, {From: at, Rates: rates(3, 3)}}},
		"second open":      {"m": {{Rates: rates(1, 1)}, {Rates: rates(2, 2)}}},
		"fractional":       {"m": {{Rates: rates(1, 1)}, {From: at.Add(500 * time.Millisecond), Rates: rates(2, 2)}}},
	}
	for name, tbl := range cases {
		if err := tbl.Validate(); err == nil || !strings.Contains(err.Error(), "m:") {
			t.Errorf("%s: want an error naming the model, got %v", name, err)
		}
	}
}

func TestLookupNormalizes(t *testing.T) {
	tbl := Default()
	for _, id := range []string{"gemini-3.5-flash", "gemini-3.5-flash-001", "gemini-3.5-flash@default"} {
		if !tbl.Known(id) {
			t.Fatalf("%s should resolve", id)
		}
	}
	// A suffix that is not a 3-digit snapshot must not strip the model apart.
	if !tbl.Known("gemini-3.5-flash-lite") {
		t.Fatal("-lite is part of the id, not a suffix")
	}
	if tbl.Known("gemini-3.1-pro-preview") {
		t.Fatal("a tiered model deliberately absent must stay unknown")
	}
	if tbl.Known("") {
		t.Fatal("empty model must be unknown")
	}
}

func TestStandardRatesCarryModifiers(t *testing.T) {
	r := StandardRates(2, 8)
	if r.InputPerMTok != 2 || r.OutputPerMTok != 8 || r.CacheReadMultiplier != 0.1 || r.NonGlobalMultiplier != 1.1 || r.GroundingPerReq != 0.014 {
		t.Fatalf("%+v", r)
	}
}
