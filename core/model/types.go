// Package model defines the data types shared across gem-usage-lens. Nothing
// here performs I/O; these types flow from collect → cost → aggregate → store.
package model

import "time"

// Source names which code path in gem-agent spent the tokens (gem-agent
// ADR-0057 §1). Every model call in the process names itself with one of
// these, so a transcript can be summed by category and priced per model.
type Source string

const (
	SourceMain          Source = "main"
	SourceRisk          Source = "risk"
	SourceProgress      Source = "progress_review"
	SourceCompact       Source = "compact"
	SourceSummarizeFile Source = "summarize_file"
	SourceWebSearch     Source = "web_search"
	SourceWebFetch      Source = "web_fetch"
	SourceFileSearch    Source = "agentic_file_search"
)

// Billable reports whether a model id can carry a cost — i.e. whether its
// absence from the rate table is a gap worth warning about. A legacy side-call
// record that never named its model has an empty id and is the intended zero.
func Billable(modelID string) bool {
	return modelID != ""
}

// LocationGlobal is the Vertex "global" endpoint, the default gem-agent uses
// for Gemini 3. Prices differ between it and every regional endpoint.
const LocationGlobal = "global"

// IsGlobal reports whether a session header's location bills at the global
// price. An empty location (a header written before ADR-0057 §4) is taken as
// global — gem-agent's default — rather than surcharged on a guess.
func IsGlobal(location string) bool {
	return location == "" || location == LocationGlobal
}

// Usage is the token breakdown of one model call, in the buckets billing
// uses (gem-agent ADR-0057 §2): Thoughts is separate from Output and bills as
// output; Cached is a discounted share of Prompt (not an addition); Total is
// the API's own count and the checksum for the other three.
type Usage struct {
	Prompt   int64
	Output   int64
	Thoughts int64
	Cached   int64
	Total    int64
}

// ChecksumOK reports whether prompt + output + thoughts == total. A record
// without a total (a legacy side-call) has nothing to check against and
// passes; the aggregator must not fail loudly on a bucket it was never given.
func (u Usage) ChecksumOK() bool {
	return u.Total == 0 || u.Prompt+u.Output+u.Thoughts == u.Total
}

// BilledTokens is the count a budget or a quota measures: everything the API
// charged for. Total when present, else the sum of the buckets. Cached is a
// share of Prompt and is not added again.
func (u Usage) BilledTokens() int64 {
	if u.Total > 0 {
		return u.Total
	}
	return u.Prompt + u.Output + u.Thoughts
}

// HasTokens reports whether the record spent anything at all.
func (u Usage) HasTokens() bool {
	return u.Prompt+u.Output+u.Thoughts > 0
}

// UsageRecord is one model call with its provenance.
type UsageRecord struct {
	// Key is the global dedup key: the transcript's path relative to the
	// sessions root plus the byte offset of the record's line. Transcripts have
	// no message id; they are append-only and a resume appends to the same
	// file, so (file, offset) identifies a call for good.
	Key       string
	Timestamp time.Time
	Host      string // local machine identity; reserved for a future multi-machine roll-up
	SessionID string // the transcript's base name — what `gem-agent --resume` takes
	Project   string // the header's project directory
	Model     string // e.g. gemini-3.7-flash
	Source    Source
	Location  string // the header's Vertex location; "" = written before it was recorded
	// Partial marks a record recovered from a transcript written before
	// gem-agent ADR-0057: its source and model were filled in from the header,
	// and the file's risk / compaction spend was never written at all, so any
	// total that includes it is a lower bound.
	Partial bool
	Usage   Usage
}

// Cost is a computed list-price-equivalent (notional) cost in USD. It is the
// Vertex AI list price, NOT an actual bill — committed-use discounts, credits
// and rounding are not modelled. Always present it as notional.
type Cost struct {
	ListPriceUSD float64
}

// PricedRecord pairs a usage record with its computed cost. This is what the
// store persists and the aggregator rolls up.
type PricedRecord struct {
	UsageRecord
	Cost Cost
}
