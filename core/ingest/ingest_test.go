package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/model"
	"github.com/nlink-jp/gem-usage-lens/core/pricing"
	"github.com/nlink-jp/gem-usage-lens/core/store"
)

const header = `{"ts":"2026-09-01T00:31:08+09:00","kind":"session","data":{"schema":2,"version":"v0.58.0","model":"gemini-3.7-flash","project":"/work/proj","location":"global"}}` + "\n"
const round = `{"ts":"2026-09-01T00:31:43+09:00","kind":"usage","data":{"source":"main","model":"gemini-3.7-flash","prompt":1000000,"output":0,"thoughts":0,"cached":0,"total":1000000}}` + "\n"
const unknown = `{"ts":"2026-09-01T00:32:43+09:00","kind":"usage","data":{"source":"main","model":"gemini-99","prompt":10,"output":1,"thoughts":0,"cached":0,"total":11}}` + "\n"

func setup(t *testing.T) (store.Store, string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "-work-proj")
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "20260901-003108.jsonl")
	os.WriteFile(p, []byte(header+round), 0o600)
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, root, p
}

func TestRunIsIncrementalAndIdempotent(t *testing.T) {
	st, root, p := setup(t)
	res, err := Run(st, root, pricing.Default(), "h")
	if err != nil || res.FilesScanned != 1 || res.FilesChanged != 1 || res.NewRecords != 1 || len(res.UnknownModels) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	recs, _ := st.Query(store.Filter{})
	if len(recs) != 1 || recs[0].Cost.ListPriceUSD != 0.75 || recs[0].Project != "/work/proj" {
		t.Fatalf("%+v", recs)
	}
	// Nothing new → nothing changes.
	res, _ = Run(st, root, pricing.Default(), "h")
	if res.FilesChanged != 0 || res.NewRecords != 0 {
		t.Fatalf("second run: %+v", res)
	}
	// Append an unknown-model round: only the new bytes are read, and the
	// unknown model is surfaced.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(unknown)
	f.Close()
	res, _ = Run(st, root, pricing.Default(), "h")
	if res.NewRecords != 1 || res.UnknownModels["gemini-99"] != 1 {
		t.Fatalf("third run: %+v", res)
	}
	recs, _ = st.Query(store.Filter{})
	if len(recs) != 2 {
		t.Fatalf("%d", len(recs))
	}
}

func TestRowsOutliveSourceFile(t *testing.T) {
	st, root, p := setup(t)
	Run(st, root, pricing.Default(), "h")
	os.Remove(p)
	Run(st, root, pricing.Default(), "h")
	if recs, _ := st.Query(store.Filter{}); len(recs) != 1 {
		t.Fatal("a deleted transcript must not delete its rows")
	}
}

func TestReprice(t *testing.T) {
	st, root, _ := setup(t)
	Run(st, root, pricing.Table{}, "h") // nothing priced
	recs, _ := st.Query(store.Filter{})
	if recs[0].Cost.ListPriceUSD != 0 {
		t.Fatal("setup")
	}
	res, err := Reprice(st, pricing.Default(), false)
	if err != nil || res.Changed != 1 || res.NewTotalUSD != 0.75 || len(res.UnknownModels) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	recs, _ = st.Query(store.Filter{})
	if recs[0].Cost.ListPriceUSD != 0.75 {
		t.Fatal("not repriced")
	}
	res, _ = Reprice(st, pricing.Table{}, true)
	if res.UnknownModels["gemini-3.7-flash"] != 1 {
		t.Fatalf("unknown must be reported on reprice too: %+v", res)
	}
	_ = model.SourceMain
}

func TestLegacyCountsSurface(t *testing.T) {
	st, root, p := setup(t)
	os.WriteFile(p, []byte(`{"ts":"2026-08-28T00:06:36+09:00","kind":"session","data":{"schema":2,"version":"v0.50.0","model":"gemini-3.7-flash","project":"/old"}}`+"\n"+
		`{"ts":"2026-08-28T00:07:22+09:00","kind":"usage","data":{"cached":0,"output":19,"prompt":43518,"thoughts":61}}`+"\n"+
		`{"ts":"2026-08-28T00:07:40+09:00","kind":"web_search","data":{"output":617,"prompt":47,"query":"q","sources":10}}`+"\n"), 0o600)
	res, err := Run(st, root, pricing.Default(), "h")
	if err != nil || res.Legacy != 1 || res.Skipped != 1 || res.NewRecords != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	recs, _ := st.Query(store.Filter{})
	if !recs[0].Partial || recs[0].Model != "gemini-3.7-flash" || recs[0].Cost.ListPriceUSD == 0 {
		t.Fatalf("legacy record must be priced from the header model and marked partial: %+v", recs[0])
	}
}

// A price change recorded as a new period (ADR-0001): reprice corrects the
// records made from the boundary on and leaves earlier ones alone, and ingest
// and reprice choose the same period for every record — including one a
// fraction of a second past the boundary, which the store keeps in seconds.
func TestRepriceAcrossAPriceChange(t *testing.T) {
	usage := func(ts string) string {
		return `{"ts":"` + ts + `","kind":"usage","data":{"source":"main","model":"gemini-3.7-flash","prompt":1000000,"output":0,"thoughts":0,"cached":0,"total":1000000}}` + "\n"
	}
	body := header +
		usage("2026-09-01T00:31:43+09:00") +
		usage("2027-01-01T16:59:59+09:00") + // 07:59:59Z — the last second of the old price
		usage("2027-01-01T17:00:00+09:00") + // 08:00:00Z — midnight Pacific, the new price
		usage("2027-01-01T17:00:00.5+09:00")
	changed := pricing.Default()
	changed["gemini-3.7-flash"] = append(changed["gemini-3.7-flash"], pricing.Period{
		From:  time.Date(2027, 1, 1, 0, 0, 0, 0, time.FixedZone("PST", -8*3600)),
		Rates: pricing.StandardRates(1.50, 7.50),
	})
	if err := changed.Validate(); err != nil {
		t.Fatal(err)
	}

	costs := func(st store.Store) []float64 {
		recs, _ := st.Query(store.Filter{})
		out := make([]float64, len(recs))
		for i, r := range recs {
			out[i] = r.Cost.ListPriceUSD
		}
		return out
	}

	// Ingested under the flat table, as every install does until the sync.
	st, root, p := setup(t)
	os.WriteFile(p, []byte(body), 0o600)
	if _, err := Run(st, root, pricing.Default(), "h"); err != nil {
		t.Fatal(err)
	}
	if got := costs(st); len(got) != 4 || got[0] != 0.75 || got[3] != 0.75 {
		t.Fatalf("flat table: %v", got)
	}
	// The sync appends the period and reprices: only the two records from the
	// boundary on move, and 2026 is left exactly as it was.
	res, err := Reprice(st, changed, false)
	if err != nil || res.Changed != 2 || res.OldTotalUSD != 3.00 || res.NewTotalUSD != 4.50 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := costs(st); got[0] != 0.75 || got[1] != 0.75 || got[2] != 1.50 || got[3] != 1.50 {
		t.Fatalf("after reprice: %v", got)
	}

	// Ingested with the period already in place, reprice has nothing to change.
	st2, root2, p2 := setup(t)
	os.WriteFile(p2, []byte(body), 0o600)
	if _, err := Run(st2, root2, changed, "h"); err != nil {
		t.Fatal(err)
	}
	if res, _ := Reprice(st2, changed, true); res.Changed != 0 {
		t.Fatalf("ingest and reprice disagree on %d record(s): %+v", res.Changed, costs(st2))
	}
}
