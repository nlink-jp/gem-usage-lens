// Package cost computes list-price-equivalent (notional) cost from usage +
// rates. Every function here is pure — no I/O, no globals.
package cost

import (
	"github.com/nlink-jp/gem-usage-lens/core/model"
	"github.com/nlink-jp/gem-usage-lens/core/pricing"
)

const perMillion = 1_000_000.0

// Compute returns the notional (Vertex AI list-price) cost of one model call.
//
// Accounting (gem-agent ADR-0057 §2):
//
//	uncached prompt = (Prompt − Cached)  × InputPerMTok
//	cached prompt   = Cached             × InputPerMTok × CacheReadMultiplier
//	output          = (Output + Thoughts) × OutputPerMTok   — thinking bills as output
//	grounding       = GroundingPerReq once, when the call was a web_search
//
// The subtotal is scaled by NonGlobalMultiplier when the session billed
// against a regional endpoint. Cached is a share of Prompt, so subtracting it
// is what keeps a cache hit from being charged twice; a Cached larger than
// Prompt (never observed) is clamped rather than produce a negative line.
func Compute(u model.Usage, r pricing.Rates, source model.Source, location string) float64 {
	perTok := func(n int64, ratePerMTok float64) float64 {
		return float64(n) / perMillion * ratePerMTok
	}
	uncached := u.Prompt - u.Cached
	if uncached < 0 {
		uncached = 0
	}
	subtotal := perTok(uncached, r.InputPerMTok) +
		perTok(u.Cached, r.InputPerMTok*r.CacheReadMultiplier) +
		perTok(u.Output+u.Thoughts, r.OutputPerMTok)
	if source == model.SourceWebSearch {
		subtotal += r.GroundingPerReq
	}
	if !model.IsGlobal(location) {
		subtotal *= r.NonGlobalMultiplier
	}
	return subtotal
}

// ComputeRecord resolves the model's rates from the table and returns a Cost.
// A record whose model is absent from the table costs 0 — which is what
// `unpriced` reporting exists to catch.
func ComputeRecord(rec model.UsageRecord, t pricing.Table) model.Cost {
	r, ok := t.Lookup(rec.Model)
	if !ok {
		return model.Cost{}
	}
	return model.Cost{ListPriceUSD: Compute(rec.Usage, r, rec.Source, rec.Location)}
}
