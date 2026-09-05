// Package aggregate rolls priced records up by one or more dimensions.
package aggregate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/model"
)

// Dimension is a group-by key.
type Dimension string

const (
	ByHour    Dimension = "hour"
	ByDay     Dimension = "day"
	ByWeek    Dimension = "week"
	ByMonth   Dimension = "month"
	BySession Dimension = "session"
	ByProject Dimension = "project"
	ByModel   Dimension = "model"
	BySource  Dimension = "source"
)

// Row is one aggregated bucket. Part of the `report --json` contract the GUI
// decodes — change in lockstep with gem-usage-lens-gui.
type Row struct {
	Key              string  `json:"key"`
	Records          int     `json:"records"`
	PromptTokens     int64   `json:"prompt_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ThoughtsTokens   int64   `json:"thoughts_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`      // the share of prompt served from cache
	ToolPromptTokens int64   `json:"tool_prompt_tokens"` // built-in tool results fed back as input (billed as input)
	TotalTokens      int64   `json:"total_tokens"`       // prompt + output + thoughts + tool prompt (the billed count)
	CostUSD          float64 `json:"cost_usd"`
	// PartialRecords counts records recovered from pre-ADR-0057 transcripts,
	// whose files never recorded risk/compaction spend: a bucket with any is
	// a lower bound.
	PartialRecords int `json:"partial_records"`
	// FirstRecord / LastRecord bound the bucket's records in time (RFC 3339,
	// whole seconds, in the aggregation location). Written always, "" when
	// no record carried a timestamp (an "unknown" bucket, a DenseTimeRows
	// filler) — no omitempty, so a consumer can tell "no timestamp" from
	// "a CLI that predates the key". For a session row they are its start
	// and last activity — since gem-agent ADR-0071 (v0.66.0) the session id
	// is a UUID and no longer says when the session began.
	FirstRecord string `json:"first_record"`
	LastRecord  string `json:"last_record"`

	first, last time.Time // the unformatted bounds, for SortRows "time"
}

// stamp formats a bound for the JSON contract: whole seconds, in loc.
func stamp(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	return t.In(loc).Truncate(time.Second).Format(time.RFC3339)
}

// Aggregate groups priced records by the given dimensions and sums tokens
// and cost. The composite key joins each dimension's value with "|". Rows are
// returned sorted by key. Pure: it takes already-priced records. Passing no
// dimensions groups by day. Time dimensions are bucketed in loc.
func Aggregate(recs []model.PricedRecord, dims []Dimension, loc *time.Location) ([]Row, error) {
	if len(dims) == 0 {
		dims = []Dimension{ByDay}
	}
	byKey := make(map[string]*Row)
	for i := range recs {
		r := &recs[i]
		key := keyFor(r, dims, loc)
		row := byKey[key]
		if row == nil {
			row = &Row{Key: key}
			byKey[key] = row
		}
		row.Records++
		row.PromptTokens += r.Usage.Prompt
		row.OutputTokens += r.Usage.Output
		row.ThoughtsTokens += r.Usage.Thoughts
		row.CachedTokens += r.Usage.Cached
		row.ToolPromptTokens += r.Usage.ToolPrompt
		row.TotalTokens += r.Usage.BilledTokens()
		row.CostUSD += r.Cost.ListPriceUSD
		if r.Partial {
			row.PartialRecords++
		}
		if !r.Timestamp.IsZero() {
			if row.first.IsZero() || r.Timestamp.Before(row.first) {
				row.first = r.Timestamp
			}
			if r.Timestamp.After(row.last) {
				row.last = r.Timestamp
			}
		}
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]Row, 0, len(keys))
	for _, k := range keys {
		row := *byKey[k]
		row.FirstRecord, row.LastRecord = stamp(row.first, loc), stamp(row.last, loc)
		out = append(out, row)
	}
	return out, nil
}

func keyFor(r *model.PricedRecord, dims []Dimension, loc *time.Location) string {
	parts := make([]string, len(dims))
	for i, d := range dims {
		parts[i] = dimValue(r, d, loc)
	}
	return strings.Join(parts, "|")
}

func dimValue(r *model.PricedRecord, d Dimension, loc *time.Location) string {
	if key, ok := timeKeyer(d, loc); ok {
		if r.Timestamp.IsZero() {
			return "unknown"
		}
		return key(r.Timestamp)
	}
	switch d {
	case BySession:
		return orUnknown(r.SessionID)
	case ByProject:
		return orUnknown(r.Project)
	case ByModel:
		return orUnknown(r.Model)
	case BySource:
		return orUnknown(string(r.Source))
	default:
		return "unknown"
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// ParseDimensions maps a comma-separated flag value to Dimensions,
// validating each. Unknown names return an error naming the offender.
func ParseDimensions(csv string) ([]Dimension, error) {
	if strings.TrimSpace(csv) == "" {
		return []Dimension{ByDay}, nil
	}
	valid := map[string]Dimension{
		"hour": ByHour, "day": ByDay, "week": ByWeek, "month": ByMonth,
		"session": BySession, "project": ByProject,
		"model": ByModel, "source": BySource,
	}
	var dims []Dimension
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		d, ok := valid[part]
		if !ok {
			return nil, fmt.Errorf("unknown group-by dimension: %s (want hour|day|week|month|session|project|model|source)", part)
		}
		dims = append(dims, d)
	}
	if len(dims) == 0 {
		dims = []Dimension{ByDay}
	}
	return dims, nil
}

// IsTimeDimension reports whether d buckets by time (and so has a
// well-defined contiguous ordering that DenseTimeRows can fill).
func IsTimeDimension(d Dimension) bool {
	_, ok := timeKeyer(d, time.UTC)
	return ok
}

// timeKeyer returns the key formatter for a time dimension, bucketed in loc.
func timeKeyer(d Dimension, loc *time.Location) (key func(time.Time) string, ok bool) {
	switch d {
	case ByHour:
		return func(t time.Time) string { return t.In(loc).Format("2006-01-02 15h") }, true
	case ByDay:
		return func(t time.Time) string { return t.In(loc).Format("2006-01-02") }, true
	case ByWeek:
		return func(t time.Time) string { y, w := t.In(loc).ISOWeek(); return fmt.Sprintf("%04d-W%02d", y, w) }, true
	case ByMonth:
		return func(t time.Time) string { return t.In(loc).Format("2006-01") }, true
	}
	return nil, false
}

// DenseTimeRows fills the gaps in a single-time-dimension roll-up so the
// series is contiguous: every bucket between start and end (inclusive) is
// present in loc, missing ones as zero rows. Existing rows are preserved
// unchanged, and out-of-range keys already present (e.g. "unknown") are kept.
// The result is sorted by key. For a non-time dimension, or when end is
// before start, rows are returned unchanged.
func DenseTimeRows(rows []Row, dim Dimension, start, end time.Time, loc *time.Location) []Row {
	key, ok := timeKeyer(dim, loc)
	if !ok || end.Before(start) {
		return rows
	}

	have := make(map[string]Row, len(rows))
	for _, r := range rows {
		have[r.Key] = r
	}

	s := start.In(loc)
	var cur time.Time
	var advance func(time.Time) time.Time
	if dim == ByHour {
		cur = time.Date(s.Year(), s.Month(), s.Day(), s.Hour(), 0, 0, 0, loc)
		advance = func(t time.Time) time.Time { return t.Add(time.Hour) }
	} else {
		cur = time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, loc)
		advance = func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	}

	seen := make(map[string]bool)
	order := make([]string, 0, len(rows))
	for !cur.After(end) {
		if k := key(cur); !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
		cur = advance(cur)
	}
	for _, r := range rows {
		if !seen[r.Key] {
			seen[r.Key] = true
			order = append(order, r.Key)
		}
	}
	sort.Strings(order)

	out := make([]Row, 0, len(order))
	for _, k := range order {
		if r, ok := have[k]; ok {
			out = append(out, r)
		} else {
			out = append(out, Row{Key: k})
		}
	}
	return out
}

// SortValues lists the accepted SortRows keys, for flag help and errors.
const SortValues = "key|time|cost|prompt|output|thoughts|cached|tokens|records"

// SortRows orders rows in place. "key" (default) sorts ascending by key;
// "time" sorts ascending by each row's first record (rows without a
// timestamp last, ties by key) — the chronological order `sessions` needs
// now that a session id no longer sorts by start time; a metric name sorts
// descending so the biggest contributors come first. "time" is not for a
// DenseTimeRows series: its filler rows have no record and would sink to
// the end (see SortConflictsWithDense).
func SortRows(rows []Row, by string) error {
	switch by {
	case "", "key":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	case "time":
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i].first, rows[j].first
			switch {
			case a.IsZero() != b.IsZero():
				return b.IsZero()
			case !a.Equal(b):
				return a.Before(b)
			}
			return rows[i].Key < rows[j].Key
		})
	case "cost":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].CostUSD > rows[j].CostUSD })
	case "prompt":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].PromptTokens > rows[j].PromptTokens })
	case "output":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].OutputTokens > rows[j].OutputTokens })
	case "thoughts":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].ThoughtsTokens > rows[j].ThoughtsTokens })
	case "cached":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].CachedTokens > rows[j].CachedTokens })
	case "tokens":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].TotalTokens > rows[j].TotalTokens })
	case "records":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Records > rows[j].Records })
	default:
		return fmt.Errorf("unknown --sort %q (want %s)", by, SortValues)
	}
	return nil
}

// SortConflictsWithDense reports the one sort that a dense time series
// cannot take: "time" would order the zero-filled gap rows after every real
// one. A time series is already chronological by key.
func SortConflictsWithDense(by string, dense bool) error {
	if dense && by == "time" {
		return fmt.Errorf("--sort time cannot be combined with --dense (filler rows have no record); a time series is chronological by key already")
	}
	return nil
}

// Summary is a period-level roll-up used by `report --summary`. Part of the
// GUI's JSON contract.
type Summary struct {
	FirstDay         string  `json:"first_day"`
	LastDay          string  `json:"last_day"`
	ActiveDays       int     `json:"active_days"`
	Records          int     `json:"records"`
	PromptTokens     int64   `json:"prompt_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ThoughtsTokens   int64   `json:"thoughts_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	ToolPromptTokens int64   `json:"tool_prompt_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	TotalUSD         float64 `json:"total_usd"`
	DailyAvgUSD      float64 `json:"daily_avg_usd"`
	PeakDay          string  `json:"peak_day"`
	PeakUSD          float64 `json:"peak_usd"`
	Projection30USD  float64 `json:"projection_30d_usd"`

	// UnpricedRecords counts the records in the period that carry tokens on a
	// billable model yet are stored at $0 — the trace a model leaves when it is
	// absent from the rate table at ingest time, or has been priced since but
	// not yet `reprice`d. UnpricedModels breaks the count down by model id.
	// Derived from the stored rows alone, with no rate table, so they are
	// correct for any config. The map is never nil (an empty object in JSON).
	UnpricedRecords int            `json:"unpriced_records"`
	UnpricedModels  map[string]int `json:"unpriced_models"`

	// PartialRecords counts records from pre-ADR-0057 transcripts (see
	// Row.PartialRecords); ChecksumMismatches counts records whose buckets do
	// not add up to the API's total — a sign of a transcript this build
	// misreads, worth a `verify`.
	PartialRecords     int `json:"partial_records"`
	ChecksumMismatches int `json:"checksum_mismatches"`
}

// Unpriced reports whether a stored record is a $0 call that should have cost
// something: it carries tokens, its model is billable, and its cost is zero.
func Unpriced(r model.PricedRecord) bool {
	return model.Billable(r.Model) && r.Cost.ListPriceUSD == 0 && r.Usage.HasTokens()
}

// Summarize computes period statistics from priced records. Totals include
// every record; day-based metrics (active days, peak, projection) ignore
// records with no timestamp. Days are bucketed in loc. The 30-day projection
// is the average cost per active day × 30.
func Summarize(recs []model.PricedRecord, loc *time.Location) Summary {
	dayRows, _ := Aggregate(recs, []Dimension{ByDay}, loc)
	s := Summary{UnpricedModels: map[string]int{}}
	for _, r := range recs {
		if Unpriced(r) {
			s.UnpricedRecords++
			s.UnpricedModels[r.Model]++
		}
		if !r.Usage.ChecksumOK() {
			s.ChecksumMismatches++
		}
	}
	for _, r := range dayRows {
		s.Records += r.Records
		s.PromptTokens += r.PromptTokens
		s.OutputTokens += r.OutputTokens
		s.ThoughtsTokens += r.ThoughtsTokens
		s.CachedTokens += r.CachedTokens
		s.ToolPromptTokens += r.ToolPromptTokens
		s.TotalTokens += r.TotalTokens
		s.TotalUSD += r.CostUSD
		s.PartialRecords += r.PartialRecords
		if r.Key == "unknown" {
			continue
		}
		s.ActiveDays++
		if s.FirstDay == "" || r.Key < s.FirstDay {
			s.FirstDay = r.Key
		}
		if r.Key > s.LastDay {
			s.LastDay = r.Key
		}
		if r.CostUSD > s.PeakUSD {
			s.PeakUSD = r.CostUSD
			s.PeakDay = r.Key
		}
	}
	if s.ActiveDays > 0 {
		s.DailyAvgUSD = s.TotalUSD / float64(s.ActiveDays)
		s.Projection30USD = s.DailyAvgUSD * 30
	}
	return s
}
