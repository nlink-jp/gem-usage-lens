package pricing

import "testing"

func TestDefaultTableAgainstPricingPage(t *testing.T) {
	// The Gemini 3 rows of the Vertex AI pricing page, global endpoint, as
	// verified on VerifiedOn. If a sync changes one of these, change the
	// expectation deliberately and bump VerifiedOn.
	want := map[string][2]float64{
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
		r, ok := tbl[m]
		if !ok {
			t.Fatalf("%s missing", m)
		}
		if r.InputPerMTok != io[0] || r.OutputPerMTok != io[1] {
			t.Fatalf("%s: %v/%v want %v/%v", m, r.InputPerMTok, r.OutputPerMTok, io[0], io[1])
		}
		// Every Gemini 3 row on the page prices cached input at 0.1× input and
		// non-global at 1.1×; grounding is $35 per 1,000 prompts.
		if r.CacheReadMultiplier != 0.1 || r.NonGlobalMultiplier != 1.1 || r.GroundingPerReq != 0.035 {
			t.Fatalf("%s modifiers: %+v", m, r)
		}
	}
	if VerifiedOn == "" {
		t.Fatal("VerifiedOn must name the day the table was checked")
	}
}

func TestLookupNormalizes(t *testing.T) {
	tbl := Default()
	for _, id := range []string{"gemini-3.5-flash", "gemini-3.5-flash-001", "gemini-3.5-flash@default"} {
		if _, ok := tbl.Lookup(id); !ok {
			t.Fatalf("%s should resolve", id)
		}
	}
	// A suffix that is not a 3-digit snapshot must not strip the model apart.
	if _, ok := tbl.Lookup("gemini-3.5-flash-lite"); !ok {
		t.Fatal("-lite is part of the id, not a suffix")
	}
	if _, ok := tbl.Lookup("gemini-3.1-pro-preview"); ok {
		t.Fatal("a tiered model deliberately absent must stay unknown")
	}
	if _, ok := tbl.Lookup(""); ok {
		t.Fatal("empty model must be unknown")
	}
}

func TestStandardRatesCarryModifiers(t *testing.T) {
	r := StandardRates(2, 8)
	if r.InputPerMTok != 2 || r.OutputPerMTok != 8 || r.CacheReadMultiplier != 0.1 || r.NonGlobalMultiplier != 1.1 || r.GroundingPerReq != 0.035 {
		t.Fatalf("%+v", r)
	}
}
