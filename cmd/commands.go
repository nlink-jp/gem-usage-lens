package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/aggregate"
	"github.com/nlink-jp/gem-usage-lens/core/collect"
	"github.com/nlink-jp/gem-usage-lens/core/config"
	"github.com/nlink-jp/gem-usage-lens/core/ingest"
	"github.com/nlink-jp/gem-usage-lens/core/model"
	"github.com/nlink-jp/gem-usage-lens/core/platform"
	"github.com/nlink-jp/gem-usage-lens/core/pricing"
	"github.com/nlink-jp/gem-usage-lens/core/store"
)

// notionalNote closes every cost display. Costs here are the Vertex AI list
// price applied to the transcript's token counts — not the bill.
const notionalNote = "Costs are a Vertex AI list-price EQUIVALENT (notional), not an actual bill."

// --- shared helpers ---

// openStore opens the durable store at the OS-standard data dir.
func openStore() (store.Store, string, error) {
	dataDir, err := platform.DataDir()
	if err != nil {
		return nil, "", err
	}
	dbPath := filepath.Join(dataDir, "usage.db")
	st, err := store.Open(dbPath)
	return st, dbPath, err
}

// configFlags holds the flags every source- or price-aware command shares,
// registered together so the precedence (flags > config file > defaults) is
// applied identically everywhere.
type configFlags struct {
	path         *string
	sessionsRoot *string
}

func registerConfigFlags(fs *flag.FlagSet, withRoot bool) *configFlags {
	cf := &configFlags{
		path: fs.String("config", "", "path to config.toml (default: the first found among the search paths; see doctor)"),
	}
	if withRoot {
		cf.sessionsRoot = fs.String("sessions-root", "", "override the gem-agent sessions root")
	}
	return cf
}

// load reads the config file named by --config (or the first found). A
// missing file is not an error — it just means "no overrides".
func (cf *configFlags) load() (*config.Config, error) {
	cfg, _, _, err := config.Load(*cf.path)
	return cfg, err
}

// root resolves the effective sessions root: flag > config > inferred.
func (cf *configFlags) root(cfg *config.Config) (string, error) {
	def, err := platform.SessionsRoot()
	if err != nil {
		return "", err
	}
	return resolveRoot(def, cfg, deref(cf.sessionsRoot)), nil
}

// resolveRoot applies the documented precedence. Pure so it is testable.
func resolveRoot(def string, cfg *config.Config, flagValue string) string {
	root := cfg.SessionsRoot(def)
	if v := strings.TrimSpace(flagValue); v != "" {
		root = v
	}
	return root
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// pricingTable returns the built-in table with the user's overrides applied.
func pricingTable(cfg *config.Config) pricing.Table {
	return cfg.PricingTable(pricing.Default())
}

// resolveTZ maps a --tz value to a location: "" / "local" → the machine's
// local time, "utc" → UTC, otherwise an IANA name. Day boundaries, "today"
// and "month" are computed in this zone; stored timestamps stay absolute.
func resolveTZ(s string) (*time.Location, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "local":
		return time.Local, nil
	case "utc":
		return time.UTC, nil
	default:
		loc, err := time.LoadLocation(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("bad --tz %q (want local | utc | an IANA name like Asia/Tokyo)", s)
		}
		return loc, nil
	}
}

// parseSince interprets a date (YYYY-MM-DD), a datetime, RFC3339, a relative
// "Nd", "today", or "month" (the 1st of the current month, 00:00), in loc.
// Empty means unbounded (0). `now` is injected for tests.
func parseSince(s string, loc *time.Location, now time.Time) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n := now.In(loc)
	switch {
	case s == "today":
		y, m, d := n.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc).Unix(), nil
	case s == "month":
		return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc).Unix(), nil
	case strings.HasSuffix(s, "d"):
		k, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("bad relative range %q (want e.g. 7d)", s)
		}
		return n.AddDate(0, 0, -k).Unix(), nil
	default:
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Unix(), nil
		}
		for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
			if t, err := time.ParseInLocation(layout, s, loc); err == nil {
				return t.Unix(), nil
			}
		}
		t, err := time.ParseInLocation("2006-01-02", s, loc)
		if err != nil {
			return 0, fmt.Errorf("bad --since %q (want YYYY-MM-DD[THH:MM[:SS]] | RFC3339 | Nd | today | month)", s)
		}
		return t.Unix(), nil
	}
}

// parseUntil is like parseSince but a bare date is treated inclusively (end
// of day) in loc; a datetime is exact.
func parseUntil(s string, loc *time.Location, now time.Time) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t.AddDate(0, 0, 1).Add(-time.Second).Unix(), nil
	}
	return parseSince(s, loc, now)
}

// --- ingest ---

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	cf := registerConfigFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := cf.load()
	if err != nil {
		return err
	}
	root, err := cf.root(cfg)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()

	st, dbPath, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	res, err := ingest.Run(st, root, pricingTable(cfg), host)
	if err != nil {
		return err
	}
	fmt.Printf("ingest complete → %s\n", dbPath)
	fmt.Printf("  files scanned: %d\n", res.FilesScanned)
	fmt.Printf("  files changed: %d\n", res.FilesChanged)
	fmt.Printf("  new records:   %d\n", res.NewRecords)
	if res.Legacy > 0 {
		fmt.Printf("  legacy:        %d record(s) from pre-ADR-0057 transcripts (their files under-count; see `verify`)\n", res.Legacy)
	}
	if res.Skipped > 0 {
		fmt.Printf("  skipped:       %d legacy side-call record(s) with no model to price\n", res.Skipped)
	}
	if res.ToolPromptDerived > 0 {
		fmt.Printf("  tool prompt:   %d record(s) carried tool-result tokens only inside total; derived and billed as input\n", res.ToolPromptDerived)
	}
	if res.ChecksumMismatches > 0 {
		fmt.Printf("  checksum:      %d record(s) where prompt+output+thoughts+tool_prompt != total (see `verify`)\n", res.ChecksumMismatches)
	}
	if res.FileErrors > 0 {
		fmt.Printf("  file errors:   %d (skipped)\n", res.FileErrors)
	}
	printUnknownModels(os.Stderr, res.UnknownModels)
	return nil
}

// printUnknownModels warns about models that carry tokens but no price.
// Without this the records land in the store at $0 and the shortfall is
// invisible. (A GUI never sees this stream — that is what the summary's
// unpriced fields are for.)
func printUnknownModels(w io.Writer, unknown map[string]int) {
	if len(unknown) == 0 {
		return
	}
	names := make([]string, 0, len(unknown))
	for m := range unknown {
		names = append(names, m)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "\nwarning: %d model(s) are not in the rate table — those records cost $0:\n", len(names))
	for _, m := range names {
		fmt.Fprintf(w, "  %-28s %d record(s)\n", m, unknown[m])
	}
	fmt.Fprintln(w, "Price them in config.toml [pricing.models] (or update the built-in table), then run `gem-usage-lens reprice`.")
}

// --- reprice ---

func runReprice(args []string) error {
	fs := flag.NewFlagSet("reprice", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "report what would change without writing")
	cf := registerConfigFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := cf.load()
	if err != nil {
		return err
	}
	st, dbPath, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	res, err := ingest.Reprice(st, pricingTable(cfg), *dryRun)
	if err != nil {
		return err
	}
	verb := "repriced"
	if *dryRun {
		verb = "would reprice"
	}
	fmt.Printf("%s → %s\n", verb, dbPath)
	fmt.Printf("  records scanned: %d\n", res.Scanned)
	fmt.Printf("  records changed: %d\n", res.Changed)
	fmt.Printf("  cost:            $%.4f → $%.4f (Δ $%+.4f)\n",
		res.OldTotalUSD, res.NewTotalUSD, res.NewTotalUSD-res.OldTotalUSD)
	printUnknownModels(os.Stderr, res.UnknownModels)
	return nil
}

// --- report ---

func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	since := fs.String("since", "", `start of range (2026-09-01 | 7d | today | month)`)
	until := fs.String("until", "", "end of range (inclusive date)")
	groupBy := fs.String("group-by", "day", "hour|day|week|month|session|project|model|source (comma-separated)")
	source := fs.String("source", "", "filter by call source (main|risk|compact|web_search|...)")
	modelFilter := fs.String("model", "", "filter by model id (substring)")
	projectFilter := fs.String("project", "", "filter by project path (substring)")
	sortBy := fs.String("sort", "", "sort rows: key|time|cost|prompt|output|thoughts|cached|tokens|records")
	top := fs.Int("top", 0, "keep only the top N rows after sorting (0 = all)")
	dense := fs.Bool("dense", false, "fill gaps in a time series with zero rows (single time group-by)")
	summary := fs.Bool("summary", false, "print period summary stats instead of rows")
	compare := fs.Bool("compare", false, "compare this period vs the preceding equal-length period (needs --since)")
	tz := fs.String("tz", "local", "timezone for day boundaries / today / month: local | utc | an IANA name")
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	loc, err := resolveTZ(*tz)
	if err != nil {
		return err
	}
	now := time.Now()
	sinceU, err := parseSince(*since, loc, now)
	if err != nil {
		return err
	}
	untilU, err := parseUntil(*until, loc, now)
	if err != nil {
		return err
	}

	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	load := func(sinceU, untilU int64) ([]model.PricedRecord, error) {
		recs, err := st.Query(store.Filter{Since: sinceU, Until: untilU, Source: model.Source(*source)})
		if err != nil {
			return nil, err
		}
		return applyFilters(recs, *modelFilter, *projectFilter), nil
	}

	recs, err := load(sinceU, untilU)
	if err != nil {
		return err
	}

	switch {
	case *compare:
		if sinceU == 0 {
			return fmt.Errorf("--compare requires --since")
		}
		end := untilU
		if end == 0 {
			end = now.Unix()
		}
		span := end - sinceU
		prev, err := load(sinceU-span, sinceU)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(buildComparison(recs, prev))
		}
		printComparison(os.Stdout, recs, prev)
		return nil

	case *summary:
		s := aggregate.Summarize(recs, loc)
		if *asJSON {
			return printJSON(s)
		}
		printSummary(os.Stdout, s)
		return nil
	}

	dims, err := aggregate.ParseDimensions(*groupBy)
	if err != nil {
		return err
	}
	rows, err := aggregate.Aggregate(recs, dims, loc)
	if err != nil {
		return err
	}
	if *dense {
		if len(dims) != 1 || !aggregate.IsTimeDimension(dims[0]) {
			return fmt.Errorf("--dense requires a single time group-by (hour|day|week|month)")
		}
		start := time.Unix(sinceU, 0).UTC()
		if sinceU == 0 {
			start = earliestTimestamp(recs)
		}
		end := now.UTC()
		if untilU != 0 {
			end = time.Unix(untilU, 0).UTC()
		}
		if !start.IsZero() {
			rows = aggregate.DenseTimeRows(rows, dims[0], start, end, loc)
		}
	}
	if *sortBy != "" {
		if err := aggregate.SortRows(rows, *sortBy); err != nil {
			return err
		}
	}
	if *top > 0 && *top < len(rows) {
		rows = rows[:*top]
	}
	if *asJSON {
		return printJSON(rows)
	}
	printReport(os.Stdout, rows, false)
	return nil
}

// earliestTimestamp returns the oldest non-zero record timestamp (UTC), or
// the zero time if none — used to bound --dense when no --since was given.
func earliestTimestamp(recs []model.PricedRecord) time.Time {
	var earliest time.Time
	for i := range recs {
		t := recs[i].Timestamp
		if t.IsZero() {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest.UTC()
}

// applyFilters narrows records by model (substring) and project (substring).
func applyFilters(recs []model.PricedRecord, mdl, project string) []model.PricedRecord {
	if mdl == "" && project == "" {
		return recs
	}
	out := make([]model.PricedRecord, 0, len(recs))
	for _, r := range recs {
		if mdl != "" && !strings.Contains(r.Model, mdl) {
			continue
		}
		if project != "" && !strings.Contains(r.Project, project) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func printSummary(w io.Writer, s aggregate.Summary) {
	period := "(no dated records)"
	if s.FirstDay != "" {
		period = s.FirstDay + " → " + s.LastDay
	}
	fmt.Fprintf(w, "period:  %s  (%d active days)\n", period, s.ActiveDays)
	fmt.Fprintf(w, "records: %d    tokens: prompt %d (cached %d) / output %d / thoughts %d / tool prompt %d / total %d\n",
		s.Records, s.PromptTokens, s.CachedTokens, s.OutputTokens, s.ThoughtsTokens, s.ToolPromptTokens, s.TotalTokens)
	fmt.Fprintf(w, "total:   $%.2f    daily avg: $%.2f\n", s.TotalUSD, s.DailyAvgUSD)
	if s.PeakDay != "" {
		fmt.Fprintf(w, "peak:    %s $%.2f    projection(30d): $%.2f\n", s.PeakDay, s.PeakUSD, s.Projection30USD)
	}
	if s.UnpricedRecords > 0 {
		fmt.Fprintf(w, "unpriced: %d record(s) stored at $0 — %s\n", s.UnpricedRecords, formatUnpriced(s.UnpricedModels))
		fmt.Fprintln(w, "  (model absent from the rate table when ingested; price it, then run `reprice`)")
	}
	if s.PartialRecords > 0 {
		fmt.Fprintf(w, "partial: %d record(s) from pre-ADR-0057 transcripts — totals are a lower bound\n", s.PartialRecords)
	}
	if s.ChecksumMismatches > 0 {
		fmt.Fprintf(w, "checksum: %d record(s) where the token buckets do not add up to total (run `verify`)\n", s.ChecksumMismatches)
	}
	fmt.Fprintln(w, "\n"+notionalNote)
}

// formatUnpriced renders an unpriced-model breakdown as "model (n), model (n)".
func formatUnpriced(models map[string]int) string {
	names := make([]string, 0, len(models))
	for m := range models {
		names = append(names, m)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, m := range names {
		parts[i] = fmt.Sprintf("%s (%d)", m, models[m])
	}
	return strings.Join(parts, ", ")
}

// periodTotals is the cross-record sum used by --compare.
type periodTotals struct {
	Records        int     `json:"records"`
	PromptTokens   int64   `json:"prompt_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	ThoughtsTokens int64   `json:"thoughts_tokens"`
	CachedTokens   int64   `json:"cached_tokens"`
	CostUSD        float64 `json:"cost_usd"`
}

func totalsOf(recs []model.PricedRecord) periodTotals {
	var t periodTotals
	for _, r := range recs {
		t.Records++
		t.PromptTokens += r.Usage.Prompt
		t.OutputTokens += r.Usage.Output
		t.ThoughtsTokens += r.Usage.Thoughts
		t.CachedTokens += r.Usage.Cached
		t.CostUSD += r.Cost.ListPriceUSD
	}
	return t
}

type comparison struct {
	Current  periodTotals `json:"current"`
	Previous periodTotals `json:"previous"`
}

func buildComparison(cur, prev []model.PricedRecord) comparison {
	return comparison{Current: totalsOf(cur), Previous: totalsOf(prev)}
}

func printComparison(w io.Writer, cur, prev []model.PricedRecord) {
	c, p := totalsOf(cur), totalsOf(prev)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tCURRENT\tPREVIOUS\tDELTA\tDELTA%")
	row := func(name string, cur, prev float64, money bool) {
		d := cur - prev
		pct := "—"
		if prev != 0 {
			pct = fmt.Sprintf("%+.1f%%", d/prev*100)
		}
		if money {
			fmt.Fprintf(tw, "%s\t$%.2f\t$%.2f\t%+.2f\t%s\n", name, cur, prev, d, pct)
		} else {
			fmt.Fprintf(tw, "%s\t%.0f\t%.0f\t%+.0f\t%s\n", name, cur, prev, d, pct)
		}
	}
	row("cost(USD)", c.CostUSD, p.CostUSD, true)
	row("prompt", float64(c.PromptTokens), float64(p.PromptTokens), false)
	row("output", float64(c.OutputTokens), float64(p.OutputTokens), false)
	row("thoughts", float64(c.ThoughtsTokens), float64(p.ThoughtsTokens), false)
	row("cached", float64(c.CachedTokens), float64(p.CachedTokens), false)
	row("records", float64(c.Records), float64(p.Records), false)
	tw.Flush()
	fmt.Fprintln(w, "\ncurrent vs. the preceding equal-length period. "+notionalNote)
}

// printReport prints aggregated rows as a table. withTimes adds STARTED and
// LAST columns (each row's first and last record, as `sessions` needs: a
// UUID session id says nothing about when the session ran).
func printReport(w io.Writer, rows []aggregate.Row, withTimes bool) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	var trec, tpartial int
	var tprompt, tout, tthoughts, tcached, ttool, ttotal int64
	var tcost float64

	times := ""
	if withTimes {
		times = "STARTED\tLAST\t"
	}
	fmt.Fprintln(tw, "KEY\t"+times+"RECORDS\tPROMPT\tCACHED\tOUTPUT\tTHOUGHTS\tTOOL\tTOTAL\tCOST(USD)")
	for _, r := range rows {
		trec += r.Records
		tpartial += r.PartialRecords
		tprompt += r.PromptTokens
		tout += r.OutputTokens
		tthoughts += r.ThoughtsTokens
		tcached += r.CachedTokens
		ttool += r.ToolPromptTokens
		ttotal += r.TotalTokens
		tcost += r.CostUSD
		mark := ""
		if r.PartialRecords > 0 {
			mark = "*"
		}
		if withTimes {
			times = tableTime(r.FirstRecord) + "\t" + tableTime(r.LastRecord) + "\t"
		}
		fmt.Fprintf(tw, "%s%s\t%s%d\t%d\t%d\t%d\t%d\t%d\t%d\t$%.4f\n", r.Key, mark, times, r.Records, r.PromptTokens, r.CachedTokens, r.OutputTokens, r.ThoughtsTokens, r.ToolPromptTokens, r.TotalTokens, r.CostUSD)
	}
	if withTimes {
		times = "\t\t"
	}
	fmt.Fprintf(tw, "TOTAL\t%s%d\t%d\t%d\t%d\t%d\t%d\t%d\t$%.4f\n", times, trec, tprompt, tcached, tout, tthoughts, ttool, ttotal, tcost)
	tw.Flush()
	if tpartial > 0 {
		fmt.Fprintf(w, "\n* %d record(s) come from pre-ADR-0057 transcripts (risk/compaction spend never recorded) — those buckets are a lower bound.\n", tpartial)
	}
	fmt.Fprintln(w, "\nCACHED is the share of PROMPT served from cache (not an addition). THOUGHTS bill at the output price. TOOL is built-in tool results (search grounding, URL context) fed back as input. "+notionalNote)
}

// tableTime shortens a Row bound (RFC 3339) to minute precision for the
// table; an empty bound (no timestamped record) prints as a dash.
func tableTime(rfc3339 string) string {
	if rfc3339 == "" {
		return "—"
	}
	if t, err := time.Parse(time.RFC3339, rfc3339); err == nil {
		return t.Format("2006-01-02 15:04")
	}
	return rfc3339
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// --- sessions ---

func runSessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	sortBy := fs.String("sort", "time", "sort rows: time|key|cost|prompt|output|thoughts|cached|tokens|records")
	top := fs.Int("top", 0, "keep only the top N rows after sorting (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	recs, err := st.Query(store.Filter{})
	if err != nil {
		return err
	}
	rows, err := aggregate.Aggregate(recs, []aggregate.Dimension{aggregate.BySession}, time.Local)
	if err != nil {
		return err
	}
	if err := aggregate.SortRows(rows, *sortBy); err != nil {
		return err
	}
	if *top > 0 && *top < len(rows) {
		rows = rows[:*top]
	}
	if *asJSON {
		return printJSON(rows)
	}
	printReport(os.Stdout, rows, true)
	return nil
}

// --- models ---

func runModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	cf := registerConfigFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := cf.load()
	if err != nil {
		return err
	}
	tbl := pricingTable(cfg)
	overridden := map[string]bool{}
	for _, m := range cfg.OverriddenModels() {
		overridden[m] = true
	}
	if *asJSON {
		return printJSON(map[string]any{"verified_on": pricing.VerifiedOn, "models": tbl})
	}
	names := make([]string, 0, len(tbl))
	for k := range tbl {
		names = append(names, k)
	}
	sort.Strings(names)

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tINPUT/Mtok\tOUTPUT/Mtok\tCACHE-READ\tGROUNDING/req\tNON-GLOBAL\tSOURCE")
	for _, m := range names {
		r := tbl[m]
		src := "built-in"
		if overridden[m] {
			src = "config"
		}
		fmt.Fprintf(tw, "%s\t$%.2f\t$%.2f\t%gx\t$%.3f\t%gx\t%s\n", m, r.InputPerMTok, r.OutputPerMTok,
			r.CacheReadMultiplier, r.GroundingPerReq, r.NonGlobalMultiplier, src)
	}
	tw.Flush()
	fmt.Printf("\nRates USD per 1M tokens on the global endpoint (built-in table verified %s against the Vertex AI pricing page).\n", pricing.VerifiedOn)
	fmt.Println("OUTPUT covers answer and thinking tokens. CACHE-READ × input is the cached-prompt price. GROUNDING is added per web_search call.")
	fmt.Println("Override via config.toml [pricing.models], then run `reprice` to apply to stored history.")
	return nil
}

// --- verify (transcript accounting check, straight from the files) ---

type verifyRow struct {
	Session           string `json:"session"`
	Path              string `json:"path"`
	HeaderFound       bool   `json:"header_found"`
	Location          string `json:"location"`
	Records           int    `json:"records"`
	Legacy            int    `json:"legacy"`
	LegacySide        int    `json:"legacy_side"`
	Skipped           int    `json:"skipped"`
	ToolPromptDerived int    `json:"tool_prompt_derived"`
	ChecksumMismatch  int    `json:"checksum_mismatch"`
	Error             string `json:"error,omitempty"`
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	all := fs.Bool("all", false, "list every file, not just the ones with findings")
	cf := registerConfigFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := cf.load()
	if err != nil {
		return err
	}
	root, err := cf.root(cfg)
	if err != nil {
		return err
	}
	files, err := collect.Discover(root)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no transcripts found under %s (run gem-agent first, or point --sessions-root at its sessions dir)", root)
	}
	host, _ := os.Hostname()

	var rows []verifyRow
	var totals verifyRow
	var partialFiles, mismatchFiles int
	for _, path := range files {
		row := verifyRow{Session: collect.SessionID(path), Path: path}
		hdr, err := collect.ReadHeader(path)
		if err == nil {
			row.HeaderFound = hdr.Found
			row.Location = hdr.Location
		}
		recs, st, perr := collect.ParseFile(path, collect.RelKey(root, path), host)
		if perr != nil {
			row.Error = perr.Error()
		}
		row.Records = len(recs)
		row.Legacy, row.LegacySide, row.Skipped, row.ChecksumMismatch = st.Legacy, st.LegacySide, st.Skipped, st.ChecksumMismatch
		row.ToolPromptDerived = st.ToolPromptDerived
		totals.Records += row.Records
		totals.Legacy += row.Legacy
		totals.LegacySide += row.LegacySide
		totals.Skipped += row.Skipped
		totals.ToolPromptDerived += row.ToolPromptDerived
		totals.ChecksumMismatch += row.ChecksumMismatch
		if row.Legacy+row.LegacySide+row.Skipped > 0 {
			partialFiles++
		}
		if row.ChecksumMismatch > 0 {
			mismatchFiles++
		}
		rows = append(rows, row)
	}

	if *asJSON {
		return printJSON(map[string]any{
			"files":          rows,
			"total_files":    len(files),
			"partial_files":  partialFiles,
			"mismatch_files": mismatchFiles,
			"totals":         totals,
		})
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tHEADER\tLOCATION\tRECORDS\tLEGACY\tSIDE\tSKIPPED\tTOOL-DERIVED\tCHECKSUM-NG\tNOTE")
	for _, r := range rows {
		finding := r.Legacy+r.LegacySide+r.Skipped+r.ToolPromptDerived+r.ChecksumMismatch > 0 || r.Error != "" || !r.HeaderFound
		if !*all && !finding {
			continue
		}
		hdr := "ok"
		if !r.HeaderFound {
			hdr = "MISSING"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n", r.Session, hdr, orDash(r.Location), r.Records, r.Legacy, r.LegacySide, r.Skipped, r.ToolPromptDerived, r.ChecksumMismatch, r.Error)
	}
	fmt.Fprintf(tw, "TOTAL (%d files)\t\t\t%d\t%d\t%d\t%d\t%d\t%d\t%d partial file(s)\n", len(files), totals.Records, totals.Legacy, totals.LegacySide, totals.Skipped, totals.ToolPromptDerived, totals.ChecksumMismatch, partialFiles)
	tw.Flush()
	fmt.Println("\nLEGACY = main-loop records written before gem-agent ADR-0057 (source/model filled from the header; risk/compaction spend was never recorded).")
	fmt.Println("SIDE = legacy side-call records taken as a lower bound; SKIPPED = legacy side-calls with no model to price.")
	fmt.Println("TOOL-DERIVED = records whose tool-result tokens (toolUsePromptTokenCount: search grounding, URL context) were present only inside total; the remainder is billed as input.")
	fmt.Println("CHECKSUM-NG = records where prompt + output + thoughts + tool prompt != total; any non-zero count means this build misreads the transcript.")
	if totals.ChecksumMismatch > 0 {
		return fmt.Errorf("%d record(s) failed the accounting checksum", totals.ChecksumMismatch)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// --- watch (near-real-time: poll + incremental ingest) ---

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	intervalStr := fs.String("interval", "5s", "poll interval (e.g. 5s, 30s, 2m)")
	cf := registerConfigFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	interval, err := time.ParseDuration(*intervalStr)
	if err != nil || interval < time.Second {
		return fmt.Errorf("bad --interval %q (min 1s)", *intervalStr)
	}
	cfg, err := cf.load()
	if err != nil {
		return err
	}
	root, err := cf.root(cfg)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	st, dbPath, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	tbl := pricingTable(cfg)

	tick := func() (newRecs, count int, total float64, err error) {
		res, err := ingest.Run(st, root, tbl, host)
		if err != nil {
			return 0, 0, 0, err
		}
		recs, err := st.Query(store.Filter{})
		if err != nil {
			return 0, 0, 0, err
		}
		for _, r := range recs {
			total += r.Cost.ListPriceUSD
		}
		return res.NewRecords, len(recs), total, nil
	}
	stamp := func() string { return time.Now().Format("15:04:05") }

	fmt.Printf("watching %s (interval %s, Ctrl-C to stop) → %s\n", root, interval, dbPath)
	_, count, total, err := tick()
	if err != nil {
		return err
	}
	fmt.Printf("[%s] baseline: %d records, total $%.2f\n", stamp(), count, total)
	prevTotal := total

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nstopped.")
			return nil
		case <-ticker.C:
			n, count, total, err := tick()
			if err != nil {
				fmt.Fprintf(os.Stderr, "ingest error: %v\n", err)
				continue
			}
			if n > 0 {
				fmt.Printf("[%s] +%d rec (Δ$%.2f)   now: %d rec / $%.2f\n", stamp(), n, total-prevTotal, count, total)
				prevTotal = total
			}
		}
	}
}

// --- daemon (register periodic ingest with the OS scheduler) ---

func runDaemon(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: gem-usage-lens daemon <install|uninstall|status> [flags]")
	}
	action, rest := args[0], args[1:]
	switch action {
	case "install":
		fs := flag.NewFlagSet("daemon install", flag.ExitOnError)
		intervalStr := fs.String("interval", "15m", "how often to ingest (e.g. 15m, 1h)")
		dryRun := fs.Bool("dry-run", false, "print the service config without installing")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		interval, err := time.ParseDuration(*intervalStr)
		if err != nil || interval < time.Minute {
			return fmt.Errorf("bad --interval %q (min 1m)", *intervalStr)
		}
		bin, err := os.Executable()
		if err != nil {
			return err
		}
		if *dryRun {
			cfg, err := platform.RenderDaemonConfig(bin, int(interval.Seconds()))
			if err != nil {
				return err
			}
			fmt.Print(cfg)
			return nil
		}
		info, err := platform.InstallDaemon(bin, int(interval.Seconds()))
		if err != nil {
			return err
		}
		fmt.Printf("installed %s daemon '%s' — runs `ingest` every %s\n  config: %s\n", info.Kind, info.Label, interval, info.ConfigPath)
		return nil
	case "uninstall":
		info, err := platform.UninstallDaemon()
		if err != nil {
			return err
		}
		fmt.Printf("removed daemon '%s'\n  (%s)\n", info.Label, info.ConfigPath)
		return nil
	case "status":
		info, err := platform.DaemonStatus()
		if err != nil {
			return err
		}
		state := "not installed"
		if info.Loaded {
			state = "loaded (running periodically)"
		} else if fileExists(info.ConfigPath) {
			state = "installed but not loaded"
		}
		fmt.Printf("daemon '%s' (%s): %s\n  config: %s\n", info.Label, info.Kind, state, info.ConfigPath)
		return nil
	default:
		return fmt.Errorf("unknown daemon action %q (want install|uninstall|status)", action)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// --- doctor (diagnoses resolved paths) ---

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cf := registerConfigFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Report the config before anything else: if it fails to parse, that is
	// the finding, and every path below would be the un-overridden default.
	cfg, cfgPath, cfgFound, cfgErr := config.Load(*cf.path)
	def, err := platform.SessionsRoot()
	if err != nil {
		return err
	}
	root, err := cf.root(cfg)
	if err != nil {
		return err
	}
	data, _ := platform.DataDir()
	searchDirs, _ := platform.ConfigSearchPaths()

	fmt.Printf("gem-usage-lens doctor (%s/%s)\n\n", runtime.GOOS, runtime.GOARCH)

	fmt.Printf("config: %s\n", cfgPath)
	switch {
	case cfgErr != nil:
		fmt.Printf("  [INVALID] %v\n", cfgErr)
	case !cfgFound:
		fmt.Println("  [absent ] using built-in defaults (every setting is optional); searched:")
		for _, d := range searchDirs {
			fmt.Printf("           %s\n", filepath.Join(d, platform.ConfigFileName))
		}
	default:
		fmt.Println("  [loaded ]")
		if models := cfg.OverriddenModels(); len(models) > 0 {
			fmt.Printf("  price overrides: %s\n", strings.Join(models, ", "))
		}
	}

	fmt.Println("\nsources:")
	reportDir("sessions", root, def)
	if v := os.Getenv(platform.StateDirEnv); v != "" {
		fmt.Printf("           %s=%s\n", platform.StateDirEnv, v)
	}
	if files, err := collect.Discover(root); err == nil {
		fmt.Printf("           %d transcript(s)\n", len(files))
	}
	fmt.Printf("\ndata:   %s\n", data)
	fmt.Println("\n(the sessions root is overridable via config [sources] sessions_root and --sessions-root)")

	// A broken config is a failure, not a note — exit non-zero so a scripted
	// health check catches it.
	return cfgErr
}

// reportDir prints the effective directory and, when it differs from the
// inferred default, says so — otherwise an override that silently failed to
// apply is indistinguishable from one that worked.
func reportDir(label, dir, def string) {
	status := "MISSING"
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		status = "ok"
	}
	fmt.Printf("  %-8s [%-7s] %s\n", label, status, dir)
	if dir != def {
		fmt.Printf("           overridden (inferred default: %s)\n", def)
	}
}
