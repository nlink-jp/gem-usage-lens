package budget

import (
	"math"
	"testing"
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/model"
)

var tokyo = mustLoad("Asia/Tokyo")

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestMonthWindowLocalCalendar(t *testing.T) {
	// 2026-09-30 23:30 UTC is already October 1st in Tokyo.
	now := time.Date(2026, 9, 30, 23, 30, 0, 0, time.UTC)
	w := MonthWindow(now, tokyo)
	if !w.Start.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, tokyo)) || !w.End.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, tokyo)) {
		t.Fatalf("Tokyo window: %v → %v", w.Start, w.End)
	}
	wu := MonthWindow(now, time.UTC)
	if !wu.Start.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) || !wu.End.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("UTC window: %v → %v", wu.Start, wu.End)
	}
	// February and December roll correctly.
	feb := MonthWindow(time.Date(2028, 2, 10, 0, 0, 0, 0, time.UTC), time.UTC)
	if feb.End.Sub(feb.Start) != 29*24*time.Hour {
		t.Fatalf("leap February: %v", feb.End.Sub(feb.Start))
	}
	dec := MonthWindow(time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), time.UTC)
	if dec.End.Year() != 2027 || dec.End.Month() != time.January {
		t.Fatalf("December end: %v", dec.End)
	}
}

func TestConsumeUsesBilledTokens(t *testing.T) {
	recs := []model.PricedRecord{
		{UsageRecord: model.UsageRecord{Usage: model.Usage{Prompt: 100, Output: 10, Thoughts: 5, Cached: 80, Total: 115}}, Cost: model.Cost{ListPriceUSD: 1.5}},
		{UsageRecord: model.UsageRecord{Usage: model.Usage{Prompt: 10, Output: 1}}, Cost: model.Cost{ListPriceUSD: 0.5}}, // legacy: no total
	}
	c := Consume(recs)
	if !near(c.USD, 2.0) || c.Tokens != 115+11 {
		t.Fatalf("%+v", c)
	}
}

func TestStateOf(t *testing.T) {
	if StateOf(50, 80, 95) != StateNormal || StateOf(80, 80, 95) != StateWarning || StateOf(94.9, 80, 95) != StateWarning || StateOf(95, 80, 95) != StateCritical || StateOf(130, 80, 95) != StateCritical {
		t.Fatal("thresholds")
	}
	if StateCritical.Rank() <= StateWarning.Rank() || StateWarning.Rank() <= StateNormal.Rank() || StateUnset.Rank() != 0 {
		t.Fatal("ranks")
	}
}

func septemberWindow() Window {
	return Window{Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

func TestProject(t *testing.T) {
	w := septemberWindow()
	mid := w.Start.Add(15 * 24 * time.Hour) // exactly half of a 30-day month

	// Half the month gone, half the budget spent → lands exactly on the limit at the reset.
	f := Project(100, 200, w, mid, 80)
	if f == nil || !near(f.Percent, 100) || !near(f.Projected, 200) || f.State != StateCritical || !f.Reliable {
		t.Fatalf("on pace: %+v", f)
	}
	if f.ExhaustionAt == nil || math.Abs(f.ExhaustionAt.Sub(w.End).Seconds()) > 1 {
		t.Fatalf("exhaustion should be the reset instant: %v", f.ExhaustionAt)
	}
	// Over pace: $150 in 15 days, $50 left at $10/day → 5 more days.
	f = Project(150, 200, w, mid, 80)
	if !near(f.Percent, 150) || f.ExhaustionAt == nil || math.Abs(f.ExhaustionAt.Sub(mid.Add(5*24*time.Hour)).Seconds()) > 1 {
		t.Fatalf("over pace: %+v", f)
	}
	// Under pace: no exhaustion, normal.
	f = Project(50, 200, w, mid, 80)
	if !near(f.Percent, 50) || f.ExhaustionAt != nil || f.State != StateNormal {
		t.Fatalf("under pace: %+v", f)
	}
	// Between warn and 100 → warning.
	if f = Project(90, 200, w, mid, 80); f.State != StateWarning {
		t.Fatalf("warning band: %+v", f)
	}
	// Already over: no exhaustion instant.
	if f = Project(250, 200, w, mid, 80); f.ExhaustionAt != nil || f.Percent < 100 {
		t.Fatalf("already over: %+v", f)
	}
	// The first sliver of the month is flagged unreliable (5% of 30 days = 36h).
	if f = Project(20, 200, w, w.Start.Add(2*time.Hour), 80); f.Reliable {
		t.Fatal("2h in must be unreliable")
	}
	if f = Project(20, 200, w, w.Start.Add(40*time.Hour), 80); !f.Reliable {
		t.Fatal("40h in must be reliable")
	}
	// Nothing to project from.
	if Project(10, 0, w, mid, 80) != nil || Project(10, 200, Window{Start: w.Start, End: w.Start}, mid, 80) != nil ||
		Project(0, 200, w, w.Start, 80) != nil || Project(10, 200, w, w.End.Add(time.Hour), 80) != nil {
		t.Fatal("degenerate inputs must yield nil")
	}
	// Zero usage projects zero.
	if f = Project(0, 200, w, mid, 80); !near(f.Projected, 0) || f.ExhaustionAt != nil {
		t.Fatalf("zero usage: %+v", f)
	}
}

func TestBuildBothBases(t *testing.T) {
	w := septemberWindow()
	now := w.Start.Add(15 * 24 * time.Hour)
	lim := Limits{USD: 100, Tokens: 0, WarnPercent: 80, CriticalPercent: 95}
	s := Build(lim, Consumption{USD: 84, Tokens: 5_000_000}, w, now, 3)
	if !s.NextReset.Equal(w.End) || !near(s.ElapsedFraction, 0.5) || s.PartialRecords != 3 {
		t.Fatalf("window: %+v", s)
	}
	if s.Cost.State != StateWarning || !near(s.Cost.Remaining, 16) || !near(s.Cost.Percent, 84) || s.Cost.Forecast == nil {
		t.Fatalf("cost basis: %+v", s.Cost)
	}
	if s.Tokens.State != StateUnset || s.Tokens.Used != 5_000_000 || s.Tokens.Remaining != 0 || s.Tokens.Percent != 0 || s.Tokens.Forecast != nil {
		t.Fatalf("unset tokens basis must report used only: %+v", s.Tokens)
	}
	// Over budget: remaining pins at 0, percent keeps counting.
	s = Build(lim, Consumption{USD: 324}, w, now, 0)
	if s.Cost.Remaining != 0 || !near(s.Cost.Percent, 324) || s.Cost.State != StateCritical {
		t.Fatalf("over: %+v", s.Cost)
	}
	// After the window (stale): elapsed pins at 1.
	if s = Build(lim, Consumption{}, w, w.End.Add(time.Hour), 0); !near(s.ElapsedFraction, 1) {
		t.Fatalf("elapsed: %v", s.ElapsedFraction)
	}
}

func TestPercentPairSumsTo100(t *testing.T) {
	for p := 0.0; p <= 200; p += 0.37 {
		u, r := PercentPair(p)
		if p <= 100 && u+r != 100 {
			t.Fatalf("p=%v → %d + %d", p, u, r)
		}
		if p > 100 && (r != 0 || u < 100) {
			t.Fatalf("over budget p=%v → %d/%d", p, u, r)
		}
	}
	if u, r := PercentPair(162); u != 162 || r != 0 {
		t.Fatalf("162 → %d/%d", u, r)
	}
}
