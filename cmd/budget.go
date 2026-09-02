package cmd

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/budget"
	"github.com/nlink-jp/gem-usage-lens/core/store"
)

// runBudget resolves the calendar-month budget: limits from flags > config
// [budget] > none, consumption from the store inside the month window in the
// chosen timezone, and the pace forecast. `--json` is the GUI's contract.
func runBudget(args []string) error {
	fs := flag.NewFlagSet("budget", flag.ExitOnError)
	limitUSD := fs.Float64("limit-usd", -1, "monthly budget in USD (overrides config [budget] monthly_usd; 0 = unset)")
	limitTokens := fs.Float64("limit-tokens", -1, "monthly budget in billed tokens (prompt+output+thoughts; 0 = unset)")
	warn := fs.Float64("warn", -1, "warning threshold percent (default 80)")
	critical := fs.Float64("critical", -1, "critical threshold percent (default 95)")
	tz := fs.String("tz", "local", "timezone the month is measured in: local | utc | an IANA name")
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	cf := registerConfigFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	loc, err := resolveTZ(*tz)
	if err != nil {
		return err
	}
	cfg, err := cf.load()
	if err != nil {
		return err
	}
	lim := cfg.BudgetLimits(budget.DefaultLimits())
	if *limitUSD >= 0 {
		lim.USD = *limitUSD
	}
	if *limitTokens >= 0 {
		lim.Tokens = *limitTokens
	}
	if *warn >= 0 {
		lim.WarnPercent = *warn
	}
	if *critical >= 0 {
		lim.CriticalPercent = *critical
	}
	if lim.WarnPercent > lim.CriticalPercent {
		return fmt.Errorf("--warn (%g) must not exceed --critical (%g)", lim.WarnPercent, lim.CriticalPercent)
	}

	now := time.Now()
	w := budget.MonthWindow(now, loc)

	st, _, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	recs, err := st.Query(store.Filter{Since: w.Start.Unix(), Until: w.End.Unix() - 1})
	if err != nil {
		return err
	}
	partial := 0
	for i := range recs {
		if recs[i].Partial {
			partial++
		}
	}
	status := budget.Build(lim, budget.Consume(recs), w, now, partial)
	if *asJSON {
		return printJSON(status)
	}
	printBudget(fs.Output(), status, loc)
	return nil
}

func printBudget(w io.Writer, s budget.Status, loc *time.Location) {
	fmt.Fprintf(w, "month:   %s → %s (%s)   elapsed %d%%\n",
		s.WindowStart.In(loc).Format("2006-01-02"), s.WindowEnd.In(loc).Format("2006-01-02"),
		loc.String(), int(s.ElapsedFraction*100+0.5))
	fmt.Fprintf(w, "cost:    %s\n", basisLine(s.Cost, moneyFmt))
	if line := forecastLine(s.Cost, moneyFmt, loc); line != "" {
		fmt.Fprintf(w, "         %s\n", line)
	}
	fmt.Fprintf(w, "tokens:  %s\n", basisLine(s.Tokens, tokenFmt))
	if line := forecastLine(s.Tokens, tokenFmt, loc); line != "" {
		fmt.Fprintf(w, "         %s\n", line)
	}
	if s.PartialRecords > 0 {
		fmt.Fprintf(w, "partial: %d record(s) in this month come from pre-ADR-0057 transcripts — used figures are a lower bound\n", s.PartialRecords)
	}
	fmt.Fprintln(w, "\nResets on the 1st at 00:00 in the zone shown. "+notionalNote)
}

func moneyFmt(v float64) string { return fmt.Sprintf("$%.2f", v) }

func tokenFmt(v float64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", v/1_000)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

// basisLine renders one basis: used against the limit with the percent pair,
// or the used amount alone plus how to set a limit.
func basisLine(b budget.Basis, f func(float64) string) string {
	if b.State == budget.StateUnset {
		return fmt.Sprintf("%s used (no limit set — pass --limit-usd/--limit-tokens or set [budget] in config.toml)", f(b.Used))
	}
	used, left := budget.PercentPair(b.Percent)
	return fmt.Sprintf("%s used / %s limit   %d%% used · %s left (%d%%)   [%s]",
		f(b.Used), f(b.Limit), used, f(b.Remaining), left, b.State)
}

// forecastLine is the one-line answer to "am I going to blow through the
// budget?", from the pace so far. Empty when there is nothing to project; the
// early-window case says so out loud rather than going quiet.
func forecastLine(b budget.Basis, f func(float64) string, loc *time.Location) string {
	fc := b.Forecast
	if fc == nil {
		return ""
	}
	if !fc.Reliable {
		return "pace: too early this month to project"
	}
	projected := fmt.Sprintf("%s (%d%%)", f(fc.Projected), int(fc.Percent+0.5))
	switch {
	case b.Used >= b.Limit:
		return "pace: over budget — on pace for " + projected
	case fc.ExhaustionAt != nil:
		return "pace: on pace for " + projected + " — budget gone " + fc.ExhaustionAt.In(loc).Format("Jan 2 15:04")
	default:
		return "pace: on pace for " + projected + " by the reset"
	}
}
