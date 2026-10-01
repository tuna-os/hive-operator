package usage

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuna-os/ccleft"
)

// This file ports the ccleft half of hive-lib.sh (2026-09-25): ccleft_probe,
// ccleft_anthropic_limits and ccleft_kiro_sample. hive-rotate.sh and
// hive-pace.sh both reduce ccleft's /readings to the "<pct_used> <note>"
// shape the old direct probes produced, so every decision downstream reads
// the same strings. Rotation diffs its plan against the bash job byte for
// byte, so the notes are reproduced exactly, not paraphrased.

// Default staleness limits (HIVE_CCLEFT_MAX_STALE_S / HIVE_CCLEFT_MAX_AGE_S).
const (
	CcleftMaxStale = 1800 * time.Second
	CcleftMaxFresh = 3600 * time.Second
)

// ProbeLine is one provider's reading in the bash probe shape: Percent is an
// integer 0-100, or -1 for UNMEASURED (never "exhausted"); Note is the rest of
// the line ("resets=…", "no-usage-api (…)", "credits=U/L resets=…").
type ProbeLine struct {
	Percent int
	Note    string
}

// String renders "<pct> <note>" as ccleft_probe prints it.
func (p ProbeLine) String() string { return strconv.Itoa(p.Percent) + " " + p.Note }

// Published renders the hive-provider-usage ConfigMap value for the line:
// "<pct>% used <note>" or "unknown <note>".
func (p ProbeLine) Published() string {
	if p.Percent == -1 {
		return strings.TrimSpace("unknown " + p.Note)
	}
	return strings.TrimSpace(fmt.Sprintf("%d%% used %s", p.Percent, p.Note))
}

// PublishedToProbe ports published_to_probe: a ConfigMap value back to a line.
func PublishedToProbe(v string) ProbeLine {
	switch {
	case strings.HasPrefix(v, "unknown"):
		note := strings.TrimPrefix(strings.TrimPrefix(v, "unknown"), " ")
		if note == "" {
			note = "unpublished"
		}
		return ProbeLine{-1, note}
	case len(v) > 0 && v[0] >= '0' && v[0] <= '9' && strings.Contains(v, "% used"):
		i := strings.Index(v, "%")
		n, err := strconv.Atoi(v[:i])
		if err != nil {
			return ProbeLine{-1, "unpublished"}
		}
		note := v[strings.Index(v, "% used")+len("% used"):]
		return ProbeLine{n, strings.TrimPrefix(note, " ")}
	}
	return ProbeLine{-1, "unpublished"}
}

// CcleftName is ccleft_provider_name: the hive-ops pool → ccleft's provider.
func CcleftName(pool string) string { return string(CcleftProvider(pool)) }

// pickReading is jq's reading($cp): the reading for that provider with the
// most homes (sort_by is stable, so the first among equals).
func pickReading(out *CcleftOutput, cp string) *ccleft.Reading {
	var best *ccleft.Reading
	for i := range out.Readings {
		r := &out.Readings[i]
		if string(r.Provider) != cp {
			continue
		}
		if best == nil || len(r.Homes) > len(best.Homes) {
			best = r
		}
	}
	return best
}

// isoSec formats a time as jq's `sec` does for a UTC ccleft timestamp.
func isoSec(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// age is jq's `age`: whole seconds since fetched_at (fractions dropped).
func readingAge(r *ccleft.Reading, now time.Time) (int64, bool) {
	if r.FetchedAt.IsZero() {
		return 0, false
	}
	return now.Unix() - r.FetchedAt.Unix(), true
}

func usableReading(r *ccleft.Reading, now time.Time, maxStale, maxFresh time.Duration) bool {
	a, ok := readingAge(r, now)
	if !ok {
		return false
	}
	if r.Stale {
		return a <= int64(maxStale/time.Second)
	}
	return a <= int64(maxFresh/time.Second)
}

// upct is jq's upct: ceil(used_pct) clamped to 0..100.
func upct(w ccleft.Window) int {
	v := int(math.Ceil(*w.UsedPct))
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return v
}

// jqNum renders a number the way jq prints it (shortest representation).
func jqNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// CcleftProbe ports ccleft_probe <provider>: the reading in the direct
// parsers' shape.
//
//	ok/limited with windows      → the worst binding window (anthropic:
//	                               unscoped only; google: the Gemini scope;
//	                               kiro: the exact credit count in the note)
//	limited/exhausted, no window → 100
//	auth_required                → 100 no-credential
//	unsupported                  → -1 no-usage-api (entry allowed)
//	error/rate_limited, no window → -1 (unmeasured: never exhausted)
//	stale beyond maxStale, or absent → -1
func CcleftProbe(out *CcleftOutput, provider string, now time.Time, maxStale, maxFresh time.Duration) ProbeLine {
	if maxStale == 0 {
		maxStale = CcleftMaxStale
	}
	if maxFresh == 0 {
		maxFresh = CcleftMaxFresh
	}
	r := pickReading(out, CcleftName(provider))
	if r == nil {
		return ProbeLine{-1, "ccleft-no-reading"}
	}
	age, hasAge := readingAge(r, now)
	cn := ""
	if r.Cause != "" {
		cn = " cause=" + r.Cause
	}
	st := ""
	if r.Stale {
		st = fmt.Sprintf(" (ccleft stale %dm%s)", floorDiv(age, 60), cn)
	}
	state := string(r.State)
	switch {
	case !hasAge:
		s := state
		if s == "" {
			s = "?"
		}
		return ProbeLine{-1, "ccleft-unmeasured state=" + s}
	case !usableReading(r, now, maxStale, maxFresh):
		return ProbeLine{-1, fmt.Sprintf("ccleft-stale age=%dm%s", floorDiv(age, 60), cn)}
	case state == "unsupported":
		return ProbeLine{-1, "no-usage-api (ccleft unsupported" + cn + ")"}
	case state == "auth_required":
		return ProbeLine{100, "no-credential (ccleft auth_required" + cn + ": needs an interactive login)"}
	}

	var pct int
	var note string
	found := false
	if provider == "kiro" {
		for _, w := range r.Windows {
			if w.Unit != "credits" || w.Limit == nil || *w.Limit <= 0 {
				continue
			}
			used := 0.0
			if w.Used != nil {
				used = *w.Used
			}
			pct = int(math.Floor(used / *w.Limit * 100))
			if pct > 100 {
				pct = 100
			}
			rs := "null"
			if w.ResetsAt != nil {
				rs = isoSec(*w.ResetsAt)
			}
			note = fmt.Sprintf("credits=%s/%s resets=%s", jqNum(math.Round(used*100)/100), jqNum(*w.Limit), rs)
			found = true
			break
		}
	} else {
		var all []ccleft.Window
		for _, w := range r.Windows {
			if w.Unit == "percent" && w.UsedPct != nil {
				all = append(all, w)
			}
		}
		var ws []ccleft.Window
		switch provider {
		case "anthropic":
			for _, w := range all {
				if w.Scope == "" {
					ws = append(ws, w)
				}
			}
		case "google":
			for _, w := range all {
				if w.Scope == "gemini" {
					ws = append(ws, w)
				}
			}
			if len(ws) == 0 {
				ws = all
			}
		default:
			ws = all
		}
		if len(ws) > 0 {
			// max_by keeps the LAST maximal element.
			b := ws[0]
			for _, w := range ws[1:] {
				if *w.UsedPct >= *b.UsedPct {
					b = w
				}
			}
			pct = upct(b)
			if provider == "openai" {
				parts := make([]string, 0, len(ws))
				for _, w := range ws {
					k := string(w.Kind)
					if k == "five_hour" {
						k = "5h"
					} else if k == "" {
						k = w.ID
					}
					parts = append(parts, fmt.Sprintf("%s=%d%%", k, upct(w)))
				}
				note = strings.Join(parts, " ") + " "
			}
			if b.ResetsAt != nil {
				note += "resets=" + isoSec(*b.ResetsAt)
			}
			if provider == "anthropic" {
				var capped []string
				for _, w := range all {
					if w.Scope != "" && *w.UsedPct >= 100 {
						capped = append(capped, w.Scope)
					}
				}
				if len(capped) > 0 {
					note += " capped-models=" + strings.Join(capped, ",")
				}
			}
			found = true
		}
	}
	limited := state == "exhausted" || state == "limited"
	if found {
		p := pct
		if limited && pct < 100 {
			p = 100
		}
		return ProbeLine{p, strings.TrimLeft(note, " ") + st}
	}
	if limited {
		msg := ""
		if r.Message != "" {
			m := r.Message
			if len([]rune(m)) > 80 {
				m = string([]rune(m)[:80])
			}
			msg = ": " + m
		}
		return ProbeLine{100, "ccleft " + state + cn + msg}
	}
	s := state
	if s == "" {
		s = "unknown"
	}
	return ProbeLine{-1, "ccleft-" + s + cn}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// LimitSlot is one unscoped Anthropic limit (hive-pace's anthropic_limits).
type LimitSlot struct {
	Slot     string
	Percent  int
	ResetsAt time.Time
}

// CcleftAnthropicLimits ports ccleft_anthropic_limits: the unscoped Claude
// percent windows with a reset, sorted by reset, as slot0, slot1, … — nil
// when the reading is unusable.
func CcleftAnthropicLimits(out *CcleftOutput, now time.Time, maxStale, maxFresh time.Duration) []LimitSlot {
	if maxStale == 0 {
		maxStale = CcleftMaxStale
	}
	if maxFresh == 0 {
		maxFresh = CcleftMaxFresh
	}
	r := pickReading(out, "claude")
	if r == nil || !usableReading(r, now, maxStale, maxFresh) {
		return nil
	}
	var ws []ccleft.Window
	for _, w := range r.Windows {
		if w.Unit == "percent" && w.UsedPct != nil && w.Scope == "" && w.ResetsAt != nil {
			ws = append(ws, w)
		}
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].ResetsAt.Unix() < ws[j].ResetsAt.Unix() })
	out2 := make([]LimitSlot, 0, len(ws))
	for i, w := range ws {
		out2 = append(out2, LimitSlot{Slot: fmt.Sprintf("slot%d", i), Percent: upct(w), ResetsAt: w.ResetsAt.UTC().Truncate(time.Second)})
	}
	if len(out2) == 0 {
		return nil
	}
	return out2
}

// PaceSample is one row of hive-pace's history (pace-history.jsonl).
type PaceSample struct {
	TS       int64
	Provider string
	Slot     string
	Pct      float64
	// Reset is the window's reset epoch, 0 when unknown.
	Reset int64
	// Used/Limit are the exact Kiro credit counts; HasUsed is false for the
	// percent-only rows of every other pool.
	Used, Limit float64
	HasUsed     bool
}

// CcleftKiroSample ports ccleft_kiro_sample: the Kiro credit pool as one
// pace-history row stamped with ccleft's FETCH time, or false.
func CcleftKiroSample(out *CcleftOutput, now time.Time, maxStale, maxFresh time.Duration) (PaceSample, bool) {
	if maxStale == 0 {
		maxStale = CcleftMaxStale
	}
	if maxFresh == 0 {
		maxFresh = CcleftMaxFresh
	}
	r := pickReading(out, "kiro")
	if r == nil || !usableReading(r, now, maxStale, maxFresh) {
		return PaceSample{}, false
	}
	for _, w := range r.Windows {
		if w.Unit != "credits" || w.Limit == nil || *w.Limit <= 0 {
			continue
		}
		used := 0.0
		if w.Used != nil {
			used = *w.Used
		}
		s := PaceSample{TS: r.FetchedAt.Unix(), Provider: "kiro", Slot: "slot0",
			Pct: math.Round(used / *w.Limit * 100000) / 1000, Used: math.Round(used*100) / 100, Limit: *w.Limit, HasUsed: true}
		if w.ResetsAt != nil {
			s.Reset = w.ResetsAt.Unix()
		}
		return s, true
	}
	return PaceSample{}, false
}

// MeasuredAt ports ccleft_measured_at: the provider reading's fetch time.
func MeasuredAt(out *CcleftOutput, provider string) (time.Time, bool) {
	r := pickReading(out, CcleftName(provider))
	if r == nil || r.FetchedAt.IsZero() {
		return time.Time{}, false
	}
	return r.FetchedAt, true
}

var (
	creditsRE = regexp.MustCompile(`credits=([0-9.]*)/([0-9.]*)`)
	resetsTok = regexp.MustCompile(`resets=([^ ]+)`)
)

// SampleFromLine ports read_limits_from_configmap: a published line → one
// slot0 sample (Kiro's exact credits when the note carries them). False when
// the line is not a measurement.
func SampleFromLine(provider string, l ProbeLine, ts int64) (PaceSample, bool) {
	if l.Percent < 0 {
		return PaceSample{}, false
	}
	s := PaceSample{TS: ts, Provider: provider, Slot: "slot0", Pct: float64(l.Percent)}
	if m := creditsRE.FindStringSubmatch(l.Note); m != nil && m[1] != "" && m[2] != "" {
		u, err1 := strconv.ParseFloat(m[1], 64)
		lim, err2 := strconv.ParseFloat(m[2], 64)
		if err1 != nil || err2 != nil || lim <= 0 {
			return PaceSample{}, false
		}
		// awk printf "%.3f"
		s.Pct, _ = strconv.ParseFloat(strconv.FormatFloat(u*100/lim, 'f', 3, 64), 64)
	}
	if m := resetsTok.FindStringSubmatch(l.Note); m != nil {
		if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			s.Reset = t.Unix()
		}
	}
	return s, true
}
