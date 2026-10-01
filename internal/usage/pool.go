package usage

import (
	"math"
	"time"
)

// Unit is what a pool's limit is expressed in.
type Unit string

const (
	// UnitCostUSD is ccusage's API-equivalent cost. The default: subscription
	// caps track compute, and cost weights Opus against Haiku and output
	// against cache reads roughly the way the providers' own meters do.
	// Raw token totals are dominated by cache reads (97% of Claude tokens
	// measured) and swing with cache hit rate, not with quota burn.
	UnitCostUSD Unit = "costUSD"
	// UnitTokens is input+output+cache read+cache create.
	UnitTokens Unit = "tokens"
	// UnitOutputTokens counts output tokens only.
	UnitOutputTokens Unit = "outputTokens"
)

// Value extracts a unit from a token breakdown.
func (u Unit) Value(t Tokens) float64 {
	switch u {
	case UnitTokens:
		return float64(t.Total)
	case UnitOutputTokens:
		return float64(t.Output)
	default:
		return t.CostUSD
	}
}

// Reading is a provider-reported percentage for one window — today the bash
// probe's hive-provider-usage ConfigMap; later hive v5's own
// /api/providers/headroom. It is the only source of "% used" that the
// provider itself vouches for, and the calibration input for learned limits.
type Reading struct {
	Percent  float64 // 0-100; <0 unknown
	ResetsAt time.Time
	At       time.Time
}

// Known reports whether the reading carries a percentage.
func (r Reading) Known() bool { return r.Percent >= 0 }

// WindowInput is everything needed to evaluate one quota window.
type WindowInput struct {
	Now      time.Time
	Duration time.Duration
	// Consumed in the window, in Unit, from window start to Now.
	Consumed float64
	// RecentConsumed over RecentSpan, for the burn rate.
	RecentConsumed float64
	RecentSpan     time.Duration
	// ConfiguredLimit (>0) wins over any learned limit.
	ConfiguredLimit float64
	// PreviousLearned is the last learned limit (0: none yet).
	PreviousLearned float64
	Reading         Reading
	// MinCalibrationPercent: readings below this are too coarse to learn from
	// (1% of a cap reported as an integer is ±50% of the estimate).
	MinCalibrationPercent float64
	// Smoothing is the EWMA weight of a new calibration (0 < a ≤ 1).
	Smoothing float64
}

// WindowResult is the evaluated window.
type WindowResult struct {
	Start, ResetsAt time.Time
	Consumed        float64
	Limit           float64
	LimitSource     string // configured | learned | none
	Learned         float64
	// Ratio is Consumed/Limit; -1 when there is no limit. Like
	// hive_provider_used_percent's -1, unknown must never read as exhausted.
	Ratio     float64
	Remaining float64 // -1 when there is no limit
	// BurnPerHour is recent consumption per hour.
	BurnPerHour float64
	// ExhaustionETA is when Remaining reaches 0 at the current burn; zero when
	// the window resets first, burn is zero, or there is no limit.
	ExhaustionETA time.Time
}

// WindowStart places a window: anchored on the provider's reset when one is
// known (the provider, not the clock, decides when a 5-hour session began),
// otherwise rolling back from now.
func WindowStart(now time.Time, d time.Duration, r Reading) (start, resets time.Time) {
	if !r.ResetsAt.IsZero() && r.ResetsAt.After(now) && r.ResetsAt.Sub(now) <= d {
		return r.ResetsAt.Add(-d), r.ResetsAt
	}
	return now.Add(-d), time.Time{}
}

// Evaluate computes a window's state. It is pure so the controller, the
// dashboard and the tests agree on the arithmetic.
func Evaluate(in WindowInput) WindowResult {
	res := WindowResult{Consumed: in.Consumed, Ratio: -1, Remaining: -1, LimitSource: "none"}
	res.Start, res.ResetsAt = WindowStart(in.Now, in.Duration, in.Reading)

	// Calibrate: consumption at p% of the cap puts the cap at c/(p/100). An
	// observed exhaustion (p ≥ 100) is the same equation at its most precise.
	res.Learned = in.PreviousLearned
	minPct := in.MinCalibrationPercent
	if minPct <= 0 {
		minPct = 5
	}
	a := in.Smoothing
	if a <= 0 || a > 1 {
		a = 0.3
	}
	if in.Reading.Known() && in.Reading.Percent >= minPct && in.Consumed > 0 {
		est := in.Consumed / (math.Min(in.Reading.Percent, 100) / 100)
		if res.Learned <= 0 {
			res.Learned = est
		} else {
			res.Learned = a*est + (1-a)*res.Learned
		}
	}

	switch {
	case in.ConfiguredLimit > 0:
		res.Limit, res.LimitSource = in.ConfiguredLimit, "configured"
	case res.Learned > 0:
		res.Limit, res.LimitSource = res.Learned, "learned"
	}

	if in.RecentSpan > 0 {
		res.BurnPerHour = in.RecentConsumed / in.RecentSpan.Hours()
	}
	if res.Limit > 0 {
		res.Ratio = in.Consumed / res.Limit
		res.Remaining = math.Max(0, res.Limit-in.Consumed)
		if res.BurnPerHour > 0 && res.Remaining > 0 {
			eta := in.Now.Add(time.Duration(res.Remaining / res.BurnPerHour * float64(time.Hour)))
			if res.ResetsAt.IsZero() || eta.Before(res.ResetsAt) {
				res.ExhaustionETA = eta
			}
		} else if res.Remaining == 0 {
			res.ExhaustionETA = in.Now
		}
	}
	return res
}

// UsedPercent is what rotation consumes: the provider's own reading when it
// has one (authoritative), else the ratio against the limit, else -1.
//
// Rotation keeps the bash semantics exactly: -1 is unmeasured and stays
// eligible; a positive reading at the threshold evacuates.
func UsedPercent(r Reading, w WindowResult) float64 {
	if r.Known() {
		return r.Percent
	}
	if w.Ratio >= 0 {
		return math.Min(100, w.Ratio*100)
	}
	return -1
}
