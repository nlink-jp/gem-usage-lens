package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/gem-usage-lens/core/model"
)

const header = `{"ts":"2026-09-01T00:31:08+09:00","kind":"session","data":{"schema":2,"version":"v0.58.0","model":"gemini-3.7-flash","project":"/work/proj","location":"global"}}` + "\n"

const modernLines = header +
	`{"ts":"2026-09-01T00:31:20+09:00","kind":"message","data":{"role":"user","content":"hi"}}` + "\n" +
	`{"ts":"2026-09-01T00:31:43+09:00","kind":"usage","data":{"source":"main","model":"gemini-3.7-flash","prompt":57068,"output":49,"thoughts":57,"cached":40000,"total":57174}}` + "\n" +
	`{"ts":"2026-09-01T00:31:50+09:00","kind":"usage","data":{"source":"risk","model":"gemini-3.7-flash","prompt":4183,"output":42,"thoughts":81,"cached":0,"total":4306}}` + "\n" +
	`{"ts":"2026-09-01T00:32:00+09:00","kind":"web_search","data":{"query":"x","sources":10}}` + "\n" +
	`{"ts":"2026-09-01T00:32:01+09:00","kind":"usage","data":{"source":"web_search","model":"gemini-3.5-flash-lite","prompt":47,"output":617,"thoughts":0,"cached":0,"total":664}}` + "\n"

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseModernTranscript(t *testing.T) {
	p := writeTemp(t, "20260901-003108.jsonl", modernLines)
	recs, st, err := ParseFile(p, "projects/x/20260901-003108.jsonl", "host1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || st.Records != 3 {
		t.Fatalf("want 3 records, got %d (stats %+v)", len(recs), st)
	}
	if st.Legacy != 0 || st.LegacySide != 0 || st.Skipped != 0 || st.ChecksumMismatch != 0 {
		t.Fatalf("unexpected stats %+v", st)
	}
	r := recs[0]
	if r.Source != model.SourceMain || r.Model != "gemini-3.7-flash" || r.Project != "/work/proj" || r.Location != "global" {
		t.Fatalf("provenance wrong: %+v", r)
	}
	if r.SessionID != "20260901-003108" || r.Host != "host1" || r.Partial {
		t.Fatalf("identity wrong: %+v", r)
	}
	if r.Usage != (model.Usage{Prompt: 57068, Output: 49, Thoughts: 57, Cached: 40000, Total: 57174}) {
		t.Fatalf("usage wrong: %+v", r.Usage)
	}
	if !strings.HasPrefix(r.Key, "projects/x/20260901-003108.jsonl@") {
		t.Fatalf("key wrong: %s", r.Key)
	}
	if r.Timestamp.IsZero() || r.Timestamp.UTC().Hour() != 15 { // 00:31 JST = 15:31 UTC previous day
		t.Fatalf("timestamp wrong: %v", r.Timestamp)
	}
	if recs[1].Source != model.SourceRisk || recs[2].Source != model.SourceWebSearch {
		t.Fatalf("sources wrong: %s %s", recs[1].Source, recs[2].Source)
	}
	// Keys are distinct byte offsets.
	if recs[0].Key == recs[1].Key || recs[1].Key == recs[2].Key {
		t.Fatalf("keys collide: %s %s %s", recs[0].Key, recs[1].Key, recs[2].Key)
	}
}

func TestParseLegacyTranscript(t *testing.T) {
	legacy := `{"ts":"2026-08-28T00:06:36+09:00","kind":"session","data":{"schema":2,"version":"v0.50.0","model":"gemini-3.7-flash","project":"/work/old"}}` + "\n" +
		`{"ts":"2026-08-28T00:07:22+09:00","kind":"usage","data":{"cached":0,"output":19,"prompt":43518,"thoughts":61}}` + "\n" +
		`{"ts":"2026-08-28T00:07:30+09:00","kind":"summary_usage","data":{"model":"gemini-3.5-flash-lite","output":492,"path":"a.md","prompt":4077}}` + "\n" +
		`{"ts":"2026-08-28T00:07:40+09:00","kind":"web_search","data":{"output":617,"prompt":47,"query":"q","sources":10}}` + "\n" +
		`{"ts":"2026-08-28T00:07:41+09:00","kind":"auto_decision","data":{"tier":"review"}}` + "\n"
	p := writeTemp(t, "20260828-000636.jsonl", legacy)
	recs, st, err := ParseFile(p, "20260828-000636.jsonl", "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records (main + summary), got %d: %+v", len(recs), recs)
	}
	if st.Legacy != 1 || st.LegacySide != 1 || st.Skipped != 1 {
		t.Fatalf("stats wrong: %+v", st)
	}
	main := recs[0]
	if main.Source != model.SourceMain || main.Model != "gemini-3.7-flash" || !main.Partial {
		t.Fatalf("legacy main not filled from header: %+v", main)
	}
	if main.Location != "" || !model.IsGlobal(main.Location) {
		t.Fatalf("legacy location should be empty and treated as global: %q", main.Location)
	}
	if main.Usage.Total != 0 || !main.Usage.ChecksumOK() {
		t.Fatalf("legacy record has no total and must pass the checksum: %+v", main.Usage)
	}
	side := recs[1]
	if side.Source != model.SourceSummarizeFile || side.Model != "gemini-3.5-flash-lite" || !side.Partial {
		t.Fatalf("legacy side-call wrong: %+v", side)
	}
	if side.Usage.Prompt != 4077 || side.Usage.Output != 492 {
		t.Fatalf("legacy side-call tokens wrong: %+v", side.Usage)
	}
}

func TestParseChecksumMismatchCounted(t *testing.T) {
	// A total BELOW the written buckets cannot be a missing bucket: it stays
	// a mismatch (a total above them is the tool-prompt bucket, derived).
	bad := header + `{"ts":"2026-09-01T00:31:43+09:00","kind":"usage","data":{"source":"main","model":"gemini-3.7-flash","prompt":10,"output":5,"thoughts":5,"cached":0,"total":7}}` + "\n"
	p := writeTemp(t, "s.jsonl", bad)
	recs, st, err := ParseFile(p, "s.jsonl", "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || st.ChecksumMismatch != 1 || st.ToolPromptDerived != 0 {
		t.Fatalf("mismatch not counted: %d recs, stats %+v", len(recs), st)
	}
	if recs[0].Usage.ChecksumOK() {
		t.Fatal("ChecksumOK must be false")
	}
}

// A web_fetch / web_search call carries the tool's results back to the model
// as toolUsePromptTokenCount, which the API adds to total but a pre-ADR-0066
// gem-agent did not write. The remainder is that bucket — exact, not a guess.
func TestParseDerivesToolPromptFromTotal(t *testing.T) {
	lines := header +
		`{"ts":"2026-09-01T08:52:29+09:00","kind":"usage","data":{"source":"web_fetch","model":"gemini-3.5-flash-lite","prompt":1200,"output":900,"thoughts":40,"cached":0,"total":9140}}` + "\n" +
		`{"ts":"2026-09-01T08:52:30+09:00","kind":"usage","data":{"source":"web_fetch","model":"gemini-3.5-flash-lite","prompt":1200,"output":900,"thoughts":40,"cached":0,"tool_prompt":7000,"total":9140}}` + "\n"
	p := writeTemp(t, "s.jsonl", lines)
	recs, st, err := ParseFile(p, "s.jsonl", "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || st.ChecksumMismatch != 0 || st.ToolPromptDerived != 1 {
		t.Fatalf("recs=%d stats=%+v", len(recs), st)
	}
	if recs[0].Usage.ToolPrompt != 7000 || !recs[0].Usage.ChecksumOK() || recs[0].Usage.BilledTokens() != 9140 {
		t.Fatalf("derived: %+v", recs[0].Usage)
	}
	// An explicit tool_prompt (gem-agent ≥ v0.62.0) is taken as written, not re-derived.
	if recs[1].Usage.ToolPrompt != 7000 || !recs[1].Usage.ChecksumOK() {
		t.Fatalf("explicit: %+v", recs[1].Usage)
	}
}

// gem-agent ADR-0066 §1: the key is written always, zero included, so its
// PRESENCE says the bucket was measured. A record that says tool_prompt:0
// and does not balance is a broken record — it must be counted as a
// checksum mismatch, not silently re-labelled "derived". Before the key was
// read as a plain int64, absent and zero were the same thing and this
// record passed as derived.
func TestParseTrustsAnExplicitZeroToolPrompt(t *testing.T) {
	lines := header +
		`{"ts":"2026-09-03T00:00:01+09:00","kind":"usage","data":{"source":"web_fetch","model":"gemini-3.5-flash-lite","prompt":186,"output":430,"thoughts":0,"cached":0,"tool_prompt":0,"total":1707}}` + "\n" +
		`{"ts":"2026-09-03T00:00:02+09:00","kind":"usage","data":{"source":"main","model":"gemini-3.7-flash","prompt":57754,"output":57,"thoughts":165,"cached":0,"tool_prompt":0,"total":57976}}` + "\n"
	p := writeTemp(t, "s.jsonl", lines)
	recs, st, err := ParseFile(p, "s.jsonl", "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || st.ToolPromptDerived != 0 || st.ChecksumMismatch != 1 {
		t.Fatalf("recs=%d stats=%+v", len(recs), st)
	}
	if recs[0].Usage.ToolPrompt != 0 || recs[0].Usage.ChecksumOK() {
		t.Fatalf("an explicit zero that does not balance was re-derived: %+v", recs[0].Usage)
	}
	if !recs[1].Usage.ChecksumOK() || recs[1].Source != model.SourceMain {
		t.Fatalf("a balanced explicit zero must pass: %+v", recs[1])
	}
}

func TestParseFromResumesAtOffsetAndLeavesTornLine(t *testing.T) {
	p := writeTemp(t, "s.jsonl", modernLines)
	recs, _, off, err := ParseFrom(p, "s.jsonl", 0, "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || off != int64(len(modernLines)) {
		t.Fatalf("first pass: %d recs, offset %d (want %d)", len(recs), off, len(modernLines))
	}

	// Append one complete record and one torn (no newline) record.
	appended := `{"ts":"2026-09-01T00:40:00+09:00","kind":"usage","data":{"source":"compact","model":"gemini-3.7-flash","prompt":100,"output":10,"thoughts":0,"cached":0,"total":110}}` + "\n"
	torn := `{"ts":"2026-09-01T00:41:00+09:00","kind":"usage","data":{"source":"main","model":"gemini-3.7-flash","prompt":1,"out`
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(appended + torn)
	f.Close()

	recs2, _, off2, err := ParseFrom(p, "s.jsonl", off, "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs2) != 1 || recs2[0].Source != model.SourceCompact {
		t.Fatalf("second pass should yield the one appended record, got %+v", recs2)
	}
	if off2 != off+int64(len(appended)) {
		t.Fatalf("offset must stop before the torn line: got %d, want %d", off2, off+int64(len(appended)))
	}
	// The resumed record still knows the header (model, project) and keys by
	// its absolute offset.
	if recs2[0].Project != "/work/proj" || recs2[0].Key != "s.jsonl@"+itoa(off) {
		t.Fatalf("resumed record provenance wrong: %+v", recs2[0])
	}

	// Complete the torn line: it is read exactly once, on the next pass.
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`put":1,"thoughts":0,"cached":0,"total":2}}` + "\n")
	f.Close()
	recs3, _, off3, err := ParseFrom(p, "s.jsonl", off2, "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs3) != 1 || recs3[0].Usage.Total != 2 {
		t.Fatalf("completed torn line not read: %+v", recs3)
	}
	fi, _ := os.Stat(p)
	if off3 != fi.Size() {
		t.Fatalf("offset should reach EOF: %d vs %d", off3, fi.Size())
	}
}

func TestParseFromRereadsWhenFileShrank(t *testing.T) {
	p := writeTemp(t, "s.jsonl", modernLines)
	recs, _, _, err := ParseFrom(p, "s.jsonl", int64(len(modernLines))+500, "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("offset past EOF must restart from 0: got %d records", len(recs))
	}
}

func TestReadHeaderMissing(t *testing.T) {
	p := writeTemp(t, "s.jsonl", `{"ts":"x","kind":"message","data":{}}`+"\n")
	h, err := ReadHeader(p)
	if err != nil || h.Found {
		t.Fatalf("no session header should give Found=false, got %+v err=%v", h, err)
	}
}

func TestDiscoverAndRelKey(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "projects", "-work-proj")
	os.MkdirAll(proj, 0o700)
	os.WriteFile(filepath.Join(proj, "20260901-003108.jsonl"), []byte(modernLines), 0o600)
	os.WriteFile(filepath.Join(proj, ".project"), []byte("/work/proj"), 0o600)
	os.WriteFile(filepath.Join(root, "legacy.jsonl"), []byte(header), 0o600)

	files, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 transcripts, got %v", files)
	}
	if k := RelKey(root, filepath.Join(proj, "20260901-003108.jsonl")); k != "projects/-work-proj/20260901-003108.jsonl" {
		t.Fatalf("RelKey: %s", k)
	}
	if SessionID(filepath.Join(proj, "20260901-003108.jsonl")) != "20260901-003108" {
		t.Fatal("SessionID")
	}
	if got, _ := Discover(filepath.Join(root, "nope")); got != nil {
		t.Fatal("missing root must yield nil, not an error")
	}
}

func TestDiscoverScanCap(t *testing.T) {
	old := maxEntriesScanned
	maxEntriesScanned = 2
	defer func() { maxEntriesScanned = old }()
	root := t.TempDir()
	for _, n := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		os.WriteFile(filepath.Join(root, n), []byte(header), 0o600)
	}
	if _, err := Discover(root); err == nil {
		t.Fatal("exceeding the scan cap must be an error, not a silent truncation")
	}
}

func itoa(n int64) string {
	return strings.TrimSpace(strings.Repeat("", 0) + formatInt(n))
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
