// Package pricing holds the per-model Vertex AI rate table. It is
// self-contained (no I/O, no other core deps) so the cost engine stays pure.
package pricing

import "strings"

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

// Table maps a model id to its Rates. The built-in Default is overridden by
// the user's config.toml [pricing.models] section at load time.
type Table map[string]Rates

// Standard modifiers, from the Vertex AI pricing page (see VerifiedOn).
const (
	cacheReadMult       = 0.10
	groundingPerReq     = 14.0 / 1000 // "$14 per 1,000 Grounding Queries" (Gemini 3 row; the $35 row is Gemini 2.x)
	nonGlobalMultiplier = 1.10
)

// VerifiedOn is the date the built-in table was last checked, column by
// column, against the Vertex AI pricing page (Gemini 3 section: global and
// non-global rows, cached-input column, grounding table). `models` prints it
// so a reader can judge how stale the table may be. The page also lists
// prices scheduled for a later date; those are deliberately not written here
// — a scheduled change is re-checked at the next sync, not assumed.
const VerifiedOn = "2026-09-03"

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
		"gemini-3.8-flash":       rates(0.75, 3.75),
		"gemini-3.7-flash":       rates(0.75, 3.75),
		"gemini-3.6-flash":       rates(0.75, 3.75),
		"gemini-3.5-flash":       rates(1.50, 9.00),
		"gemini-3.5-flash-lite":  rates(0.30, 2.50),
		"gemini-3.1-flash-lite":  rates(0.25, 1.50),
		"gemini-3-flash-preview": rates(0.50, 3.00),
	}
}

// Lookup returns the rates for a model and whether it is known. It tries an
// exact match, then the id with a trailing "@<version>" or a "-NNN" numeric
// snapshot suffix removed ("gemini-3.5-flash-001" → "gemini-3.5-flash").
func (t Table) Lookup(model string) (Rates, bool) {
	for _, c := range candidates(model) {
		if r, ok := t[c]; ok {
			return r, true
		}
	}
	return Rates{}, false
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
