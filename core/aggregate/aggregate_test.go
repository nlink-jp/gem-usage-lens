package aggregate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/model"
)

func rec(ts string, sess, mdl string, src model.Source, u model.Usage, cost float64, partial bool) model.PricedRecord {
	t, _ := time.Parse(time.RFC3339, ts)
	return model.PricedRecord{
		UsageRecord: model.UsageRecord{Timestamp: t, SessionID: sess, Project: "/p", Model: mdl, Source: src, Usage: u, Partial: partial},
		Cost:        model.Cost{ListPriceUSD: cost},
	}
}

func sample() []model.PricedRecord {
	return []model.PricedRecord{
		rec("2026-09-01T10:00:00Z", "s1", "gemini-3.7-flash", model.SourceMain, model.Usage{Prompt: 100, Output: 10, Thoughts: 5, Cached: 60, Total: 115}, 1.0, false),
		rec("2026-09-01T11:00:00Z", "s1", "gemini-3.7-flash", model.SourceRisk, model.Usage{Prompt: 50, Output: 5, Thoughts: 0, Cached: 0, Total: 55}, 0.5, false),
		rec("2026-09-03T09:00:00Z", "s2", "gemini-3.5-flash-lite", model.SourceWebSearch, model.Usage{Prompt: 10, Output: 20}, 0.0, true),
	}
}

func TestAggregateCarriesToolPrompt(t *testing.T) {
	r := rec("2026-09-03T09:00:00Z", "s", "m", model.SourceWebFetch, model.Usage{Prompt: 100, Output: 10, ToolPrompt: 500, Total: 610}, 0.1, false)
	rows, _ := Aggregate([]model.PricedRecord{r}, []Dimension{ByDay}, time.UTC)
	if rows[0].ToolPromptTokens != 500 || rows[0].TotalTokens != 610 {
		t.Fatalf("%+v", rows[0])
	}
	if s := Summarize([]model.PricedRecord{r}, time.UTC); s.ToolPromptTokens != 500 || s.ChecksumMismatches != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestAggregateByDayAndSource(t *testing.T) {
	rows, err := Aggregate(sample(), []Dimension{ByDay}, time.UTC)
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %+v", err, rows)
	}
	d1 := rows[0]
	if d1.Key != "2026-09-01" || d1.Records != 2 || d1.PromptTokens != 150 || d1.OutputTokens != 15 || d1.ThoughtsTokens != 5 || d1.CachedTokens != 60 || d1.TotalTokens != 170 || d1.CostUSD != 1.5 || d1.PartialRecords != 0 {
		t.Fatalf("day1: %+v", d1)
	}
	d3 := rows[1]
	if d3.Key != "2026-09-03" || d3.TotalTokens != 30 || d3.PartialRecords != 1 {
		t.Fatalf("day3 (legacy, no total → summed buckets): %+v", d3)
	}
	bySrc, _ := Aggregate(sample(), []Dimension{BySource}, time.UTC)
	if len(bySrc) != 3 || bySrc[0].Key != "main" || bySrc[1].Key != "risk" || bySrc[2].Key != "web_search" {
		t.Fatalf("by source: %+v", bySrc)
	}
	composite, _ := Aggregate(sample(), []Dimension{ByDay, ByModel}, time.UTC)
	if composite[0].Key != "2026-09-01|gemini-3.7-flash" {
		t.Fatalf("composite key: %s", composite[0].Key)
	}
}

func TestTimeBucketsFollowLocation(t *testing.T) {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	// 2026-09-01T20:00Z is 2026-09-02 05:00 in Tokyo.
	r := rec("2026-09-01T20:00:00Z", "s", "m", model.SourceMain, model.Usage{Prompt: 1, Total: 1}, 0.1, false)
	rows, _ := Aggregate([]model.PricedRecord{r}, []Dimension{ByDay}, tokyo)
	if rows[0].Key != "2026-09-02" {
		t.Fatalf("Tokyo day: %s", rows[0].Key)
	}
	rows, _ = Aggregate([]model.PricedRecord{r}, []Dimension{ByMonth}, tokyo)
	if rows[0].Key != "2026-09" {
		t.Fatalf("month: %s", rows[0].Key)
	}
}

func TestParseDimensions(t *testing.T) {
	d, err := ParseDimensions("day, model")
	if err != nil || len(d) != 2 || d[0] != ByDay || d[1] != ByModel {
		t.Fatalf("%v %v", d, err)
	}
	if _, err := ParseDimensions("entrypoint"); err == nil {
		t.Fatal("entrypoint is not a gem dimension")
	}
	if d, _ := ParseDimensions(""); len(d) != 1 || d[0] != ByDay {
		t.Fatal("default is day")
	}
}

func TestDenseTimeRows(t *testing.T) {
	rows, _ := Aggregate(sample(), []Dimension{ByDay}, time.UTC)
	start, _ := time.Parse(time.RFC3339, "2026-08-31T00:00:00Z")
	end, _ := time.Parse(time.RFC3339, "2026-09-04T00:00:00Z")
	dense := DenseTimeRows(rows, ByDay, start, end, time.UTC)
	keys := make([]string, len(dense))
	for i, r := range dense {
		keys[i] = r.Key
	}
	want := []string{"2026-08-31", "2026-09-01", "2026-09-02", "2026-09-03", "2026-09-04"}
	if len(keys) != len(want) {
		t.Fatalf("%v", keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("%v", keys)
		}
	}
	if dense[2].CostUSD != 0 || dense[1].CostUSD != 1.5 {
		t.Fatal("filled rows must be zero and existing rows preserved")
	}
	if got := DenseTimeRows(rows, ByModel, start, end, time.UTC); len(got) != len(rows) {
		t.Fatal("non-time dimension must be a no-op")
	}
}

func TestSortRows(t *testing.T) {
	rows, _ := Aggregate(sample(), []Dimension{ByModel}, time.UTC)
	if err := SortRows(rows, "cost"); err != nil || rows[0].Key != "gemini-3.7-flash" {
		t.Fatalf("%v %+v", err, rows)
	}
	if err := SortRows(rows, "tokens"); err != nil || rows[0].Key != "gemini-3.7-flash" {
		t.Fatal("tokens sort")
	}
	if err := SortRows(rows, "cache"); err == nil {
		t.Fatal("'cache' is not a gem sort key (it is 'cached')")
	}
}

func TestSummarizeAndUnpriced(t *testing.T) {
	s := Summarize(sample(), time.UTC)
	if s.FirstDay != "2026-09-01" || s.LastDay != "2026-09-03" || s.ActiveDays != 2 || s.Records != 3 {
		t.Fatalf("%+v", s)
	}
	if s.TotalUSD != 1.5 || s.DailyAvgUSD != 0.75 || s.PeakDay != "2026-09-01" || s.Projection30USD != 22.5 {
		t.Fatalf("%+v", s)
	}
	if s.TotalTokens != 200 || s.CachedTokens != 60 || s.ThoughtsTokens != 5 {
		t.Fatalf("tokens: %+v", s)
	}
	// The $0 flash-lite web_search record carries tokens on a billable model.
	if s.UnpricedRecords != 1 || s.UnpricedModels["gemini-3.5-flash-lite"] != 1 {
		t.Fatalf("unpriced: %+v", s)
	}
	if s.PartialRecords != 1 {
		t.Fatalf("partial: %+v", s)
	}
	// A record with no model (legacy side-call that was still stored somehow)
	// or with no tokens is not unpriced.
	if Unpriced(rec("2026-09-01T00:00:00Z", "s", "", model.SourceMain, model.Usage{Prompt: 1}, 0, true)) {
		t.Fatal("empty model is not billable")
	}
	if Unpriced(rec("2026-09-01T00:00:00Z", "s", "m", model.SourceMain, model.Usage{}, 0, false)) {
		t.Fatal("no tokens is not unpriced")
	}
	empty := Summarize(nil, time.UTC)
	if empty.UnpricedModels == nil {
		t.Fatal("UnpricedModels must never be nil (JSON contract: {} not null)")
	}
}

func TestSummarizeCountsChecksumMismatches(t *testing.T) {
	bad := rec("2026-09-01T00:00:00Z", "s", "m", model.SourceMain, model.Usage{Prompt: 10, Output: 1, Total: 99}, 0.1, false)
	if s := Summarize([]model.PricedRecord{bad}, time.UTC); s.ChecksumMismatches != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestRowsCarryFirstAndLastRecord(t *testing.T) {
	rows, _ := Aggregate(sample(), []Dimension{BySession}, time.UTC)
	if len(rows) != 2 || rows[0].Key != "s1" {
		t.Fatalf("%+v", rows)
	}
	if rows[0].FirstRecord != "2026-09-01T10:00:00Z" || rows[0].LastRecord != "2026-09-01T11:00:00Z" {
		t.Fatalf("s1 bounds: %q %q", rows[0].FirstRecord, rows[0].LastRecord)
	}
	if rows[1].FirstRecord != "2026-09-03T09:00:00Z" || rows[1].LastRecord != rows[1].FirstRecord {
		t.Fatalf("s2 bounds: %q %q", rows[1].FirstRecord, rows[1].LastRecord)
	}
	// Bounds follow the aggregation location, whole seconds.
	jst := time.FixedZone("JST", 9*3600)
	rows, _ = Aggregate(sample(), []Dimension{BySession}, jst)
	if rows[0].FirstRecord != "2026-09-01T19:00:00+09:00" {
		t.Fatalf("in JST: %q", rows[0].FirstRecord)
	}
	// A record without a timestamp leaves the bounds empty rather than 0001 —
	// and present: the keys are written always (no omitempty).
	none := rec("", "s3", "m", model.SourceMain, model.Usage{Prompt: 1, Total: 1}, 0, false)
	rows, _ = Aggregate([]model.PricedRecord{none}, []Dimension{BySession}, time.UTC)
	if rows[0].FirstRecord != "" || rows[0].LastRecord != "" {
		t.Fatalf("no timestamp: %+v", rows[0])
	}
	b, _ := json.Marshal(rows[0])
	if !strings.Contains(string(b), `"first_record":""`) || !strings.Contains(string(b), `"last_record":""`) {
		t.Fatalf("keys must be present even when empty: %s", b)
	}
}

func TestSortTimeRefusedWithDense(t *testing.T) {
	// A dense series' filler rows carry no record, so "time" would sink
	// them to the end of an otherwise chronological series.
	if err := SortConflictsWithDense("time", true); err == nil {
		t.Fatal("dense + time must be refused")
	}
	for _, by := range []string{"key", "cost", ""} {
		if err := SortConflictsWithDense(by, true); err != nil {
			t.Fatalf("%q with dense: %v", by, err)
		}
	}
	if err := SortConflictsWithDense("time", false); err != nil {
		t.Fatal("time without dense is fine")
	}
	rows, _ := Aggregate(sample(), []Dimension{ByDay}, time.UTC)
	start, _ := time.Parse(time.RFC3339, "2026-09-01T00:00:00Z")
	end, _ := time.Parse(time.RFC3339, "2026-09-03T00:00:00Z")
	dense := DenseTimeRows(rows, ByDay, start, end, time.UTC)
	_ = SortRows(dense, "time")
	if dense[len(dense)-1].Key != "2026-09-02" {
		t.Fatalf("the conflict the guard exists for: %v", dense)
	}
}

func TestSortRowsByTime(t *testing.T) {
	// A UUID id (gem-agent ADR-0071) sorts by key after every timestamp id
	// and in hex order among UUIDs; "time" restores chronological order.
	recs := []model.PricedRecord{
		rec("2026-09-05T03:00:00Z", "f5f26494-4547-486a-939e-a52e203a038b", "m", model.SourceMain, model.Usage{Prompt: 1, Total: 1}, 0, false),
		rec("2026-09-05T01:00:00Z", "20260905-100000", "m", model.SourceMain, model.Usage{Prompt: 1, Total: 1}, 0, false),
		rec("2026-09-05T02:00:00Z", "2ebb723d-0712-4fcc-bf4d-7745d637e70a", "m", model.SourceMain, model.Usage{Prompt: 1, Total: 1}, 0, false),
		rec("", "no-timestamp", "m", model.SourceMain, model.Usage{Prompt: 1, Total: 1}, 0, false),
	}
	rows, _ := Aggregate(recs, []Dimension{BySession}, time.UTC)
	if err := SortRows(rows, "time"); err != nil {
		t.Fatal(err)
	}
	want := []string{"20260905-100000", "2ebb723d-0712-4fcc-bf4d-7745d637e70a", "f5f26494-4547-486a-939e-a52e203a038b", "no-timestamp"}
	for i, k := range want {
		if rows[i].Key != k {
			t.Fatalf("order: %v", rows)
		}
	}
	if err := SortRows(rows, "key"); err != nil || rows[0].Key != "20260905-100000" || rows[1].Key != "2ebb723d-0712-4fcc-bf4d-7745d637e70a" {
		t.Fatalf("key order kept: %v %v", err, rows)
	}
}
