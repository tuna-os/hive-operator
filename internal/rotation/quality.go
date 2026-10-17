package rotation

import (
	"math"
	"strconv"
	"strings"
)

// ScoreMeets requires finite, explicitly supplied scores when a floor is set.
// An invalid floor fails closed; an absent floor preserves legacy eligibility.
func ScoreMeets(score, minimum string) bool {
	if minimum == "" {
		return true
	}
	floor, err := strconv.ParseFloat(minimum, 64)
	if err != nil || math.IsNaN(floor) || math.IsInf(floor, 0) {
		return false
	}
	value, err := strconv.ParseFloat(score, 64)
	return err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= floor
}

// ModelEffort resolves a rung's launch syntax. Agy's suffix and pi/omp's
// :thinking carry the desired effort; observedEffort handles overriding flags.
func ModelEffort(backend, model, effort string) string {
	if e := AgyEffort(backend, model); e != "" {
		return e
	}
	if backend == "pi" || backend == "omp" {
		if _, e, ok := strings.Cut(model, ":"); ok && contains([]string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}, e) {
			return e
		}
	}
	return effort
}

// EffortValue returns the launch effort encoded by this exact rung.
func (r Rung) EffortValue() string { return ModelEffort(r.Backend, r.Model, r.Effort) }

// PlacementEffort preserves legacy launch behavior, but clears an omp effort
// flag when the selector already carries the same thinking level.
func (r Rung) PlacementEffort() string {
	return placementEffort(r.Backend, r.Model, r.Effort)
}

func placementEffort(backend, model, effort string) string {
	if backend == "omp" {
		if encoded := ModelEffort(backend, model, ""); encoded != "" && (effort == "" || effort == encoded) {
			return ""
		}
	}
	if e := AgyEffort(backend, model); e != "" {
		return e
	}
	return effort
}

func (r Rung) eligible(minimum string) bool {
	if !ScoreMeets(r.Score, minimum) {
		return false
	}
	if minimum == "" {
		return true
	}
	effort := r.EffortValue()
	return effort != "" && (r.Effort == "" || r.Effort == effort)
}

func observedEffort(backend, model, effort string) string {
	if backend == "pi" || (backend == "omp" && effort == "") {
		return ModelEffort(backend, model, effort)
	}
	return effort
}

// PlacementAllowed checks the one effective ladder, independent of tier: pace
// demotions may legitimately sit outside the agent's original tier.
func (in Input) PlacementAllowed(backend, model, effort string) bool {
	if in.MinimumScore == "" {
		return true
	}
	actual := observedEffort(backend, model, effort)
	for _, r := range in.Rungs {
		if r.Backend == backend && r.Model == model && r.eligible(in.MinimumScore) && r.EffortValue() == actual {
			return true
		}
	}
	return false
}

func (a FleetAgent) allowed(backend, model, effort string) bool {
	return (Input{Rungs: a.Eligible, MinimumScore: a.MinimumScore}).PlacementAllowed(backend, model, effort)
}

func (a FleetAgent) cheaper() string {
	model := RungDown(a.Model)
	if model != "" && !a.allowed(a.CLI, model, ModelEffort(a.CLI, model, a.Effort)) {
		return ""
	}
	return model
}

func hasPin(pins map[string]PinTarget, agent string) bool {
	_, ok := pins[agent]
	return ok
}

func (a FleetAgent) destinationEffort(backend, model string) string {
	effort := ""
	if a.MinimumScore != "" {
		effort = a.Effort
	}
	return placementEffort(backend, model, effort)
}
