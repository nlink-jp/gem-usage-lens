package ingest

import (
	"os"
	"path/filepath"
	"testing"

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
