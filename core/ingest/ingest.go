// Package ingest orchestrates bringing the durable store up to date from the
// transcripts: discover → parse (incrementally) → price → upsert.
package ingest

import (
	"os"

	"github.com/nlink-jp/gem-usage-lens/core/collect"
	"github.com/nlink-jp/gem-usage-lens/core/cost"
	"github.com/nlink-jp/gem-usage-lens/core/model"
	"github.com/nlink-jp/gem-usage-lens/core/pricing"
	"github.com/nlink-jp/gem-usage-lens/core/store"
)

// Result summarizes an ingest run.
type Result struct {
	FilesScanned int
	FilesChanged int
	NewRecords   int
	FileErrors   int

	// Legacy counts records recovered from pre-ADR-0057 transcripts on this
	// run (their files under-count); Skipped counts legacy side-call records
	// that had no model to price with; ChecksumMismatches counts records whose
	// buckets did not add up to the API's total.
	Legacy             int
	Skipped            int
	ChecksumMismatches int
	// ToolPromptDerived counts records whose tool-use prompt tokens were
	// filled from the checksum remainder (gem-agent does not write the bucket).
	ToolPromptDerived int

	// UnknownModels counts the records just ingested whose model is absent
	// from the rate table, keyed by model id. Such records are stored at $0,
	// so surfacing them is what turns a silent under-count into a visible
	// warning. Add the model (config or table) and run Reprice to fix history.
	UnknownModels map[string]int
}

// RepriceResult reports a Reprice pass: the store-level totals plus any
// models still missing from the rate table (those rows stay at $0).
type RepriceResult struct {
	store.RepriceResult
	UnknownModels map[string]int
}

// Reprice recomputes the cost of every stored record against tbl, without
// re-reading any transcript. Ingest is incremental — already-consumed bytes
// are never revisited and Upsert is DO NOTHING — so a rate-table change
// otherwise applies only to future records. This applies it to the past too.
func Reprice(st store.Store, tbl pricing.Table, dryRun bool) (RepriceResult, error) {
	unknown := map[string]int{}
	sr, err := st.Reprice(func(rec model.UsageRecord) model.Cost {
		if _, known := tbl.Lookup(rec.Model); !known && model.Billable(rec.Model) {
			unknown[rec.Model]++
		}
		return cost.ComputeRecord(rec, tbl)
	}, dryRun)
	return RepriceResult{RepriceResult: sr, UnknownModels: unknown}, err
}

// Run brings the store up to date from the sessions root. host stamps
// provenance on every record. Incremental: only appended bytes are read;
// upserts are idempotent; a file error skips that file and continues.
func Run(st store.Store, root string, tbl pricing.Table, host string) (Result, error) {
	var res Result
	files, err := collect.Discover(root)
	if err != nil {
		return res, err
	}
	for _, path := range files {
		res.FilesScanned++
		offset, _, err := st.IngestState(path)
		if err != nil {
			return res, err
		}
		recs, stats, newOffset, err := collect.ParseFrom(path, collect.RelKey(root, path), offset, host)
		if err != nil {
			res.FileErrors++
			continue
		}
		if newOffset == offset && len(recs) == 0 {
			continue
		}
		res.FilesChanged++
		res.Legacy += stats.Legacy + stats.LegacySide
		res.Skipped += stats.Skipped
		res.ChecksumMismatches += stats.ChecksumMismatch
		res.ToolPromptDerived += stats.ToolPromptDerived

		priced := make([]model.PricedRecord, len(recs))
		for i, r := range recs {
			if _, known := tbl.Lookup(r.Model); !known && model.Billable(r.Model) {
				if res.UnknownModels == nil {
					res.UnknownModels = map[string]int{}
				}
				res.UnknownModels[r.Model]++
			}
			priced[i] = model.PricedRecord{UsageRecord: r, Cost: cost.ComputeRecord(r, tbl)}
		}
		n, err := st.Upsert(priced)
		if err != nil {
			return res, err
		}
		res.NewRecords += n
		var mtime int64
		if fi, err := os.Stat(path); err == nil {
			mtime = fi.ModTime().Unix()
		}
		if err := st.SetIngestState(path, newOffset, mtime, newOffset); err != nil {
			return res, err
		}
	}
	return res, nil
}
