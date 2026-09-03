package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/model"
)

func openTemp(t *testing.T) (Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "data", "usage.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, p
}

func priced(key string, ts time.Time, src model.Source, u model.Usage, cost float64, partial bool) model.PricedRecord {
	return model.PricedRecord{
		UsageRecord: model.UsageRecord{Key: key, Timestamp: ts, Host: "h", SessionID: "s1", Project: "/p", Model: "gemini-3.7-flash", Source: src, Location: "global", Partial: partial, Usage: u},
		Cost:        model.Cost{ListPriceUSD: cost},
	}
}

func TestUpsertIsIdempotentAndQueryRoundTrips(t *testing.T) {
	st, _ := openTemp(t)
	ts := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	recs := []model.PricedRecord{
		priced("a.jsonl@10", ts, model.SourceMain, model.Usage{Prompt: 100, Output: 10, Thoughts: 5, Cached: 60, Total: 115}, 1.25, false),
		priced("a.jsonl@200", ts.Add(time.Hour), model.SourceRisk, model.Usage{Prompt: 50, Output: 5, Total: 55}, 0.5, true),
	}
	n, err := st.Upsert(recs)
	if err != nil || n != 2 {
		t.Fatalf("first upsert: %d %v", n, err)
	}
	n, err = st.Upsert(recs)
	if err != nil || n != 0 {
		t.Fatalf("re-upsert must insert nothing: %d %v", n, err)
	}

	got, err := st.Query(Filter{})
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %d", err, len(got))
	}
	r := got[0]
	if r.Key != "a.jsonl@10" || r.Source != model.SourceMain || r.Location != "global" || r.Partial || r.Cost.ListPriceUSD != 1.25 {
		t.Fatalf("round trip: %+v", r)
	}
	if r.Usage != (model.Usage{Prompt: 100, Output: 10, Thoughts: 5, Cached: 60, Total: 115}) {
		t.Fatalf("usage: %+v", r.Usage)
	}
	if !r.Timestamp.Equal(ts) {
		t.Fatalf("timestamp: %v", r.Timestamp)
	}
	if !got[1].Partial {
		t.Fatal("partial flag lost")
	}

	// Filters: time range and source.
	if got, _ = st.Query(Filter{Since: ts.Add(30 * time.Minute).Unix()}); len(got) != 1 || got[0].Key != "a.jsonl@200" {
		t.Fatalf("since filter: %+v", got)
	}
	if got, _ = st.Query(Filter{Until: ts.Add(30 * time.Minute).Unix()}); len(got) != 1 || got[0].Key != "a.jsonl@10" {
		t.Fatalf("until filter: %+v", got)
	}
	if got, _ = st.Query(Filter{Source: model.SourceRisk}); len(got) != 1 {
		t.Fatalf("source filter: %+v", got)
	}
}

func TestUpsertRejectsEmptyKey(t *testing.T) {
	st, _ := openTemp(t)
	if _, err := st.Upsert([]model.PricedRecord{priced("", time.Now(), model.SourceMain, model.Usage{}, 0, false)}); err == nil {
		t.Fatal("an empty key would collapse every keyless record onto one row")
	}
}

func TestRepriceRewritesFromTokenColumns(t *testing.T) {
	st, _ := openTemp(t)
	ts := time.Now()
	st.Upsert([]model.PricedRecord{
		priced("k1", ts, model.SourceMain, model.Usage{Prompt: 1_000_000, Total: 1_000_000}, 0, false), // stored at $0 (unpriced at the time)
		priced("k2", ts, model.SourceWebSearch, model.Usage{Prompt: 10, Total: 10}, 0.5, false),
	})
	price := func(r model.UsageRecord) model.Cost {
		if r.Source == model.SourceWebSearch {
			return model.Cost{ListPriceUSD: 0.5} // unchanged
		}
		return model.Cost{ListPriceUSD: float64(r.Usage.Prompt) / 1e6 * 0.75}
	}
	dry, err := st.Reprice(price, true)
	if err != nil || dry.Scanned != 2 || dry.Changed != 1 || dry.OldTotalUSD != 0.5 || dry.NewTotalUSD != 1.25 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	got, _ := st.Query(Filter{})
	if got[0].Cost.ListPriceUSD != 0 && got[1].Cost.ListPriceUSD != 0 {
		t.Fatal("dry run must not write")
	}
	res, err := st.Reprice(price, false)
	if err != nil || res.Changed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	got, _ = st.Query(Filter{})
	sum := got[0].Cost.ListPriceUSD + got[1].Cost.ListPriceUSD
	if sum != 1.25 {
		t.Fatalf("after reprice: %v", sum)
	}
}

func TestIngestState(t *testing.T) {
	st, _ := openTemp(t)
	if off, ok, err := st.IngestState("/x"); err != nil || ok || off != 0 {
		t.Fatalf("unknown path: %d %v %v", off, ok, err)
	}
	if err := st.SetIngestState("/x", 100, 1, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIngestState("/x", 250, 2, 250); err != nil {
		t.Fatal(err)
	}
	if off, ok, _ := st.IngestState("/x"); !ok || off != 250 {
		t.Fatalf("%d %v", off, ok)
	}
}

func TestOpenTightensPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes only")
	}
	_, p := openTemp(t)
	if fi, _ := os.Stat(filepath.Dir(p)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir perms %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("db perms %v", fi.Mode().Perm())
	}
}

// A store created by v0.1.0 has no tool_prompt_tokens column and holds rows
// whose total exceeds the written buckets. Open must add the column, Query
// must derive the bucket on read, and Reprice must persist it.
func TestMigrationFromV010DerivesToolPrompt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	old := `CREATE TABLE usage_records (record_key TEXT PRIMARY KEY, ts INTEGER, host TEXT, session_id TEXT,
	 project TEXT, model TEXT, source TEXT, location TEXT, prompt_tokens INTEGER, output_tokens INTEGER,
	 thoughts_tokens INTEGER, cached_tokens INTEGER, total_tokens INTEGER, checksum_ok INTEGER, partial INTEGER,
	 cost_usd REAL, ingested_at INTEGER);
	 INSERT INTO usage_records VALUES ('k', 1, 'h', 's', '/p', 'gemini-3.7-flash', 'web_fetch', 'global',
	 1200, 900, 40, 0, 9140, 0, 0, 0.5, 1);`
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.Query(Filter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %d", err, len(got))
	}
	if got[0].Usage.ToolPrompt != 7000 || !got[0].Usage.ChecksumOK() {
		t.Fatalf("derived on read: %+v", got[0].Usage)
	}
	res, err := st.Reprice(func(r model.UsageRecord) model.Cost {
		return model.Cost{ListPriceUSD: float64(r.Usage.ToolPrompt) / 1e6}
	}, false)
	if err != nil || res.Changed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	var tool int64
	var ok int
	db, _ = sql.Open("sqlite", p)
	defer db.Close()
	if err := db.QueryRow("SELECT tool_prompt_tokens, checksum_ok FROM usage_records WHERE record_key='k'").Scan(&tool, &ok); err != nil {
		t.Fatal(err)
	}
	if tool != 7000 || ok != 1 {
		t.Fatalf("reprice must persist the derived bucket and the checksum verdict: %d %d", tool, ok)
	}
}

func TestReopenKeepsRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	st.Upsert([]model.PricedRecord{priced("k", time.Now(), model.SourceMain, model.Usage{Prompt: 1, Total: 1}, 0.1, false)})
	st.Close()
	st2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if got, _ := st2.Query(Filter{}); len(got) != 1 {
		t.Fatal("rows must survive reopen (schema is IF NOT EXISTS)")
	}
}
