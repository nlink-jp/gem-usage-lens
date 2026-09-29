// Package pricing holds the per-model Vertex AI rate table. It is
// self-contained (no I/O, no other core deps) so the cost engine stays pure.
package pricing

import (
	"fmt"
	"strings"
	"time"
)

// Rates are USD prices per 1,000,000 tokens for one model, plus the
// modifiers the cost engine applies.
type Rates struct {
	InputPerMTok  float64 `json:"input_per_mtok"`  // prompt tokens that were not served from cache
	OutputPerMTok float64 `json:"output_per_mtok"` // output AND thinking tokens ("answer and reasoning" share one price)

	// CacheReadMultiplier × InputPerMTok is the price of a cached prompt
	// token. 0.1 for every Gemini 3 model on the pricing page today, but kept
	// per model: the one footnoted exception is what breaks a constant.
	CacheReadMultiplier float64 `json:"cache_read_multiplier"`

	// GroundingPerReq is the per-request charge for Grounding with Google
	// Search, added once per web_search call. It is invisible in the token
	// counts, which is why it is a separate field (gem-agent ADR-0057 lesson).
	// The page bills per Grounding *Query* and one prompt may issue several,
	// so one charge per call is a lower bound; the 5,000 free queries per
	// month are not modelled (list price, not the invoice).
	GroundingPerReq float64 `json:"grounding_per_req"`

	// NonGlobalMultiplier scales the whole cost when the session billed
	// against a regional endpoint instead of "global" (the pricing page's
	// "non-global" column is 1.1× across the board).
	NonGlobalMultiplier float64 `json:"non_global_multiplier"`
}

// Period is the rates in force from From onward, until the next period of the
// same model starts. The zero From opens the first period from the beginning
// of time (ADR-0001).
type Period struct {
	From time.Time
	Rates
}

// Table maps a model id to its periods, ordered by From, the first one open.
// A model whose price has never changed has exactly one period. The built-in
// Default is overridden by the user's config.toml [pricing.models] section at
// load time.
type Table map[string][]Period

// Flat is the period list of a model priced the same at every date.
func Flat(r Rates) []Period { return []Period{{Rates: r}} }

// Standard modifiers, from the Vertex AI pricing page (see VerifiedOn).
const (
	cacheReadMult       = 0.10
	groundingPerReq     = 14.0 / 1000 // "$14 per 1,000 Grounding Queries" (Gemini 3 row; the $35 row is Gemini 2.x)
	nonGlobalMultiplier = 1.10
)

// VerifiedOn is the date the built-in table was last checked, column by
// column, against the Vertex AI pricing page (Gemini 3 section: global and
// non-global rows, cached-input column, grounding table). `models` prints it
// so a reader can judge how stale the table may be.
//
// The table states only prices in force on VerifiedOn. The page also lists
// prices scheduled for a later date (3.8 / 3.7 / 3.6 Flash from 2027-01-01);
// those are not written ahead of time, because a scheduled change can be
// cancelled. Once a sync confirms it happened, it is APPENDED as a period
// starting at midnight US Pacific Time on its date — written as a literal
// RFC 3339 instant — never written over the old price, so `reprice` corrects
// records made after the change and leaves earlier ones alone (ADR-0001).
const VerifiedOn = "2026-09-29"

// StandardRates returns a Rates for the given base prices with the standard
// modifiers. It is the starting point for a model defined purely in the
// user's config, so two prices still yield correct cache and region handling.
func StandardRates(input, output float64) Rates { return rates(input, output) }

func rates(input, output float64) Rates {
	return Rates{
		InputPerMTok:        input,
		OutputPerMTok:       output,
		CacheReadMultiplier: cacheReadMult,
		GroundingPerReq:     groundingPerReq,
		NonGlobalMultiplier: nonGlobalMultiplier,
	}
}

// Default returns the built-in rate table: USD per 1M tokens on the global
// endpoint, verified on VerifiedOn. Override or extend via config.toml
// [pricing.models]. Unknown models are absent by design → zero cost, and
// ingest / report surface them as unpriced.
//
// Flash-class models have no long-context premium (the ≤200k and >200k
// columns carry the same price), so one flat rate per model is exact.
// Tiered models (3.1 Pro preview: a higher rate above 200k prompt tokens) are
// not in the table — the cost engine has no prompt-length tier, so pricing
// them flat would silently under-count long prompts.
func Default() Table {
	return Table{
		"gemini-3.8-flash":       Flat(rates(0.75, 3.75)),
		"gemini-3.7-flash":       Flat(rates(0.75, 3.75)),
		"gemini-3.6-flash":       Flat(rates(0.75, 3.75)),
		"gemini-3.5-flash":       Flat(rates(1.50, 9.00)),
		"gemini-3.5-flash-lite":  Flat(rates(0.30, 2.50)),
		"gemini-3.1-flash-lite":  Flat(rates(0.25, 1.50)),
		"gemini-3-flash-preview": Flat(rates(0.50, 3.00)),
	}
}

// Periods returns a model's periods and whether it is known. It tries an
// exact match, then the id with a trailing "@<version>" or a "-NNN" numeric
// snapshot suffix removed ("gemini-3.5-flash-001" → "gemini-3.5-flash").
func (t Table) Periods(model string) ([]Period, bool) {
	for _, c := range candidates(model) {
		if ps, ok := t[c]; ok && len(ps) > 0 {
			return ps, true
		}
	}
	return nil, false
}

// Known reports whether a model is priced at all, at any date. Whether a
// model is known never depends on time, so the unknown-model warnings and
// the unpriced counts use this rather than Lookup.
func (t Table) Known(model string) bool {
	_, ok := t.Periods(model)
	return ok
}

// Lookup returns the rates in force for a model at the instant at: the last
// period whose From is at or before it, so a call made exactly at a period's
// start takes the new price. A zero at (a record without a timestamp) falls
// in the first period, which is open from the beginning.
func (t Table) Lookup(model string, at time.Time) (Rates, bool) {
	ps, ok := t.Periods(model)
	if !ok {
		return Rates{}, false
	}
	r := ps[0].Rates
	for _, p := range ps[1:] {
		if p.From.After(at) {
			break
		}
		r = p.Rates
	}
	return r, true
}

// Validate checks the invariants every pricing path relies on: each model has
// at least one period, the first is open (zero From), the rest start at
// strictly increasing instants, and every start is a whole second — the store
// keeps timestamps in whole seconds, and a fractional start would let ingest
// (full precision) and reprice (stored seconds) pick different periods.
func (t Table) Validate() error {
	for m, ps := range t {
		if len(ps) == 0 {
			return fmt.Errorf("%s: no price period", m)
		}
		if !ps[0].From.IsZero() {
			return fmt.Errorf("%s: the first period must be open (it starts at %s)", m, ps[0].From.Format(time.RFC3339))
		}
		for i := 1; i < len(ps); i++ {
			if ps[i].From.IsZero() || !ps[i].From.After(ps[i-1].From) {
				return fmt.Errorf("%s: period %d must start after period %d", m, i+1, i)
			}
			if ps[i].From.Nanosecond() != 0 {
				return fmt.Errorf("%s: period %d starts at a fractional second (%s)", m, i+1, ps[i].From.Format(time.RFC3339Nano))
			}
		}
	}
	return nil
}

// Current returns, for every model, the rates in force at now.
func (t Table) Current(now time.Time) map[string]Rates {
	out := make(map[string]Rates, len(t))
	for m := range t {
		out[m], _ = t.Lookup(m, now)
	}
	return out
}

func candidates(m string) []string {
	out := []string{m}
	if i := strings.IndexByte(m, '@'); i > 0 {
		out = append(out, m[:i])
	}
	if b := stripNumericSuffix(m); b != m {
		out = append(out, b)
	}
	return out
}

// stripNumericSuffix removes a trailing "-NNN" (three digits) snapshot suffix.
func stripNumericSuffix(m string) string {
	i := strings.LastIndexByte(m, '-')
	if i <= 0 || len(m)-i != 4 {
		return m
	}
	for _, c := range m[i+1:] {
		if c < '0' || c > '9' {
			return m
		}
	}
	return m[:i]
}
