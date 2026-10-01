package usage

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ProbeReading is one provider line of the bash-published ConfigMap
// hive/hive-provider-usage, parsed the way published_to_probe (hive-lib.sh)
// parses it:
//
//	"<N>% used <note>"  → Percent N
//	"unknown <note>"    → Percent -1
//	anything else       → Percent -1, note "unpublished"
type ProbeReading struct {
	Percent  float64
	Note     string
	ResetsAt time.Time
	// Slots are Anthropic's unscoped limits, from anthropic_limits, ordered by
	// resets_at so slot0 is the session (5h) window and slot1 the weekly one.
	Slots map[string]Reading
}

var resetsRE = regexp.MustCompile(`resets=(\S+)`)

// ParseProbeValue parses one provider value.
func ParseProbeValue(v string) ProbeReading {
	v = strings.TrimSpace(v)
	r := ProbeReading{Percent: -1}
	f := strings.Fields(v)
	switch {
	case len(f) >= 2 && strings.HasSuffix(f[0], "%") && f[1] == "used":
		if n, err := strconv.ParseFloat(strings.TrimSuffix(f[0], "%"), 64); err == nil {
			r.Percent = n
		}
		r.Note = strings.Join(f[2:], " ")
	case len(f) >= 1 && f[0] == "unknown":
		r.Note = strings.Join(f[1:], " ")
	default:
		r.Note = "unpublished"
	}
	if m := resetsRE.FindStringSubmatch(r.Note); m != nil {
		if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			r.ResetsAt = t.UTC()
		}
	}
	return r
}

// ParseProbeConfigMap parses the whole ConfigMap. Readings carry the
// ConfigMap's updated_at so staleness can be judged by the caller — the bash
// rotate jobs reuse a publication for up to 20 minutes; a consumer must not
// treat a day-old reading as current.
func ParseProbeConfigMap(data map[string]string) (map[string]ProbeReading, time.Time) {
	var at time.Time
	if t, err := time.Parse(time.RFC3339, data["updated_at"]); err == nil {
		at = t.UTC()
	}
	out := map[string]ProbeReading{}
	for k, v := range data {
		switch k {
		case "updated_at", "measured_by", "anthropic_limits":
			continue
		}
		out[k] = ParseProbeValue(v)
	}
	if raw, ok := data["anthropic_limits"]; ok {
		var limits []struct {
			Slot     string   `json:"slot"`
			Percent  *float64 `json:"percent"`
			ResetsAt string   `json:"resets_at"`
		}
		if json.Unmarshal([]byte(raw), &limits) == nil {
			a := out["anthropic"]
			a.Slots = map[string]Reading{}
			sort.SliceStable(limits, func(i, j int) bool { return limits[i].ResetsAt < limits[j].ResetsAt })
			for _, l := range limits {
				if l.Percent == nil {
					continue
				}
				rd := Reading{Percent: *l.Percent, At: at}
				if t, err := time.Parse(time.RFC3339Nano, l.ResetsAt); err == nil {
					rd.ResetsAt = t.UTC()
				}
				a.Slots[l.Slot] = rd
			}
			out["anthropic"] = a
		}
	}
	return out, at
}

// Reading converts a probe line (or one of its slots) into a Reading.
func (p ProbeReading) Reading(slot string, at time.Time) Reading {
	if slot != "" {
		if r, ok := p.Slots[slot]; ok {
			return r
		}
		return Reading{Percent: -1}
	}
	return Reading{Percent: p.Percent, ResetsAt: p.ResetsAt, At: at}
}
