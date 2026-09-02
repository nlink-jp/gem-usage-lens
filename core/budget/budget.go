// Package budget is the calendar-month budget arithmetic: the window, the
// consumption inside it, the threshold state, and a pace forecast. Every
// function is pure so the CLI's `budget --json` — the GUI's only source of
// these figures — is fully unit-tested here and nowhere else.
package budget

import (
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/model"
)

// Limits is the budget the user set. A zero USD or Tokens limit means "not
// set" on that basis; that basis then reports StateUnset rather than 0%.
type Limits struct {
	USD             float64
	Tokens          float64
	WarnPercent     float64
	CriticalPercent float64
}

// DefaultLimits has no amounts set and the conventional thresholds.
func DefaultLimits() Limits {
	return Limits{WarnPercent: 80, CriticalPercent: 95}
}

// Window is one calendar month in the caller's timezone.
type Window struct {
	Start time.Time // the 1st, 00:00 local
	End   time.Time // the next 1st, 00:00 local (exclusive)
}

// MonthWindow returns the calendar month containing now, in loc. Calendar
// arithmetic (AddDate) so a DST change inside the month keeps the boundary
// at local midnight.
func MonthWindow(now time.Time, loc *time.Location) Window {
	n := now.In(loc)
	start := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	return Window{Start: start, End: start.AddDate(0, 1, 0)}
}

// Consumption is what the records inside the window spent.
type Consumption struct {
	USD    float64
	Tokens int64 // the billed count: prompt + output + thoughts
}

// Consume sums the records. The caller has already restricted them to the
// window (a store query by timestamp); this only adds.
func Consume(recs []model.PricedRecord) Consumption {
	var c Consumption
	for i := range recs {
		c.USD += recs[i].Cost.ListPriceUSD
		c.Tokens += recs[i].Usage.BilledTokens()
	}
	return c
}

// State is the threshold state of one basis.
type State string

const (
	StateUnset    State = "unset" // no limit on this basis
	StateNormal   State = "normal"
	StateWarning  State = "warning"
	StateCritical State = "critical"
)

// Rank orders states by severity, for "did it get worse" comparisons.
func (s State) Rank() int {
	switch s {
	case StateWarning:
		return 1
	case StateCritical:
		return 2
	default:
		return 0
	}
}

// StateOf maps a used percentage to a state: normal < warn ≤ warning <
// critical ≤ critical.
func StateOf(percent, warnPercent, criticalPercent float64) State {
	if percent >= criticalPercent {
		return StateCritical
	}
	if percent >= warnPercent {
		return StateWarning
	}
	return StateNormal
}

// Forecast is a linear burn-rate projection: where the month lands if usage
// keeps flowing at the average rate observed so far.
type Forecast struct {
	// Projected is the usage extrapolated to the window end.
	Projected float64 `json:"projected"`
	// Percent is Projected as a share of the limit — may exceed 100.
	Percent float64 `json:"percent"`
	// ExhaustionAt is when the limit is reached at this rate, if that instant
	// falls inside the window. nil = not before the reset, or already over.
	ExhaustionAt *time.Time `json:"exhaustion_at"`
	// Reliable is false in the first sliver of the window, where a single
	// session dominates the average and the extrapolation is noise. The UI
	// says so rather than presenting a wild number as a forecast.
	Reliable bool `json:"reliable"`
	// State is the severity of the projection itself: critical at ≥100% of the
	// limit regardless of the user's critical threshold (a projected overrun
	// always reads as one), warning at ≥ the warning threshold.
	State State `json:"state"`
}

// minimumElapsedFraction guards the noisy head of the window: 5% of a month
// is about 36 hours.
const minimumElapsedFraction = 0.05

// Project extrapolates used over the elapsed slice of [start, end] to the
// whole window. Returns nil when there is nothing to project from — no
// limit, a degenerate window, or now outside it.
func Project(used, limit float64, w Window, now time.Time, warnPercent float64) *Forecast {
	total := w.End.Sub(w.Start).Seconds()
	elapsed := now.Sub(w.Start).Seconds()
	if limit <= 0 || total <= 0 || elapsed <= 0 || elapsed > total {
		return nil
	}
	fraction := elapsed / total
	projected := used / fraction
	percent := projected / limit * 100

	// Whole seconds: the JSON contract is RFC 3339 without fractions, which
	// every consumer (the GUI's ISO 8601 decoder included) reads.
	var exhaustion *time.Time
	rate := used / elapsed
	if used < limit && rate > 0 {
		hit := now.Add(time.Duration((limit - used) / rate * float64(time.Second))).Truncate(time.Second)
		if !hit.After(w.End) {
			exhaustion = &hit
		}
	}
	return &Forecast{
		Projected:    projected,
		Percent:      percent,
		ExhaustionAt: exhaustion,
		Reliable:     fraction >= minimumElapsedFraction,
		State:        StateOf(percent, warnPercent, 100),
	}
}

// Basis is one budget basis (USD or tokens) resolved against the window.
type Basis struct {
	Limit     float64   `json:"limit"`
	Used      float64   `json:"used"`
	Remaining float64   `json:"remaining"` // max(0, limit − used); 0 when unset
	Percent   float64   `json:"percent"`   // used ÷ limit × 100; 0 when unset
	State     State     `json:"state"`
	Forecast  *Forecast `json:"forecast"` // nil when unset or unprojectable
}

// Status is the `budget --json` payload: the window and both bases.
type Status struct {
	WindowStart     time.Time `json:"window_start"`
	WindowEnd       time.Time `json:"window_end"`
	NextReset       time.Time `json:"next_reset"` // == WindowEnd; named for what the reader wants
	ElapsedFraction float64   `json:"elapsed_fraction"`
	Cost            Basis     `json:"cost"`
	Tokens          Basis     `json:"tokens"`
	// PartialRecords is carried so a reader knows the used figures are a lower
	// bound when legacy transcripts fall inside the window.
	PartialRecords int `json:"partial_records"`
}

// Build resolves limits against consumption for the window at now.
func Build(lim Limits, c Consumption, w Window, now time.Time, partial int) Status {
	total := w.End.Sub(w.Start).Seconds()
	elapsed := now.Sub(w.Start).Seconds()
	fraction := 0.0
	if total > 0 && elapsed > 0 {
		fraction = min(elapsed/total, 1)
	}
	return Status{
		WindowStart:     w.Start,
		WindowEnd:       w.End,
		NextReset:       w.End,
		ElapsedFraction: fraction,
		Cost:            basis(lim.USD, c.USD, lim, w, now),
		Tokens:          basis(lim.Tokens, float64(c.Tokens), lim, w, now),
		PartialRecords:  partial,
	}
}

func basis(limit, used float64, lim Limits, w Window, now time.Time) Basis {
	b := Basis{Limit: limit, Used: used, State: StateUnset}
	if limit <= 0 {
		return b
	}
	b.Remaining = max(0, limit-used)
	b.Percent = used / limit * 100
	b.State = StateOf(b.Percent, lim.WarnPercent, lim.CriticalPercent)
	b.Forecast = Project(used, limit, w, now, lim.WarnPercent)
	return b
}

// PercentPair returns whole percents for display, derived together so
// "42% used · 58% left" always sums to 100 — rounding the two independently
// can print 42/57. Over budget the used side keeps counting past 100 and the
// remainder pins at 0.
func PercentPair(percent float64) (used, remaining int) {
	used = int(percent + 0.5)
	if percent < 0 {
		used = 0
	}
	remaining = max(0, 100-used)
	return used, remaining
}
