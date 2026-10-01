package rotation

import "strings"

// RungDown ports rung_down (hive-lib.sh): the pacer's one-notch-cheaper rung
// on the SAME provider and backend, or "". Shared by pace (demote) and
// placement (which must treat a pacer-demoted rung as legitimate, or the two
// fight).
//
// Kiro credits scale with the model's rateMultiplier (ListAvailableModels,
// 2026-09-24): opus-5 2.2, sonnet-5 1.3, haiku-4.5 0.4, gpt-5.6 sol 4.4 /
// terra 2.2 / luna 1.1. The first Kiro notch keeps pi's `:<thinking>` suffix;
// the second (sonnet-5 / luna → haiku-4.5) is always `:low`, the exact T3 rung.
func RungDown(model string) string {
	switch model {
	case "claude-fable-5-1", "claude-fable-5", "claude-opus-5-5", "claude-opus-5":
		return "claude-sonnet-5"
	case "gemini-3.8-flash-high":
		return "gemini-3.8-flash-low"
	case "gemini-3.7-flash-high":
		return "gemini-3.7-flash-low"
	case "gpt-6-astra", "gpt-5.6-sol":
		return "gpt-5.6-luna"
	}
	const k = "kiro-api-key/"
	for _, pair := range [][2]string{
		{k + "claude-opus-5", k + "claude-sonnet-5"},
		{k + "gpt-5-6-sol", k + "gpt-5-6-luna"},
		{k + "gpt-5-6-terra", k + "gpt-5-6-luna"},
	} {
		if model == pair[0] || strings.HasPrefix(model, pair[0]+":") {
			return pair[1] + strings.TrimPrefix(model, pair[0])
		}
	}
	for _, base := range []string{k + "claude-sonnet-5", k + "gpt-5-6-luna"} {
		if model == base || strings.HasPrefix(model, base+":") {
			return k + "claude-haiku-4-5:low"
		}
	}
	return ""
}

// RungChain ports rung_chain: the model, then every RungDown below it.
func RungChain(model string) []string {
	var out []string
	for i := 0; model != "" && i < 6; i++ {
		out = append(out, model)
		model = RungDown(model)
	}
	return out
}

// RungUpToward ports rung_up_toward: the rung one notch ABOVE current on
// original's demotion chain, or "" when current is not below original on it.
func RungUpToward(original, current string) string {
	prev := ""
	for _, m := range RungChain(original) {
		if m == current {
			return prev
		}
		prev = m
	}
	return ""
}

// demotedFrom: is current a rung BELOW original on its chain (any notch)?
func demotedFrom(original, current string) bool {
	ch := RungChain(original)
	for _, m := range ch[min(1, len(ch)):] {
		if m == current {
			return true
		}
	}
	return false
}

// KiroCreditMult ports kiro_credit_mult: Kiro credits per request.
func KiroCreditMult(model string) float64 {
	switch {
	case strings.Contains(model, "gpt-5-6-sol"):
		return 4.4
	case strings.Contains(model, "gpt-5-6-terra"):
		return 2.2
	case strings.Contains(model, "gpt-5-6-luna"):
		return 1.1
	case strings.Contains(model, "claude-opus-"):
		return 2.2
	case strings.Contains(model, "claude-sonnet-"):
		return 1.3
	case strings.Contains(model, "claude-haiku-"):
		return 0.4
	}
	return 1.0
}

// AgyEffort ports agy_effort_of: the --effort an agy model id REQUIRES (agy
// silently runs Gemini 3.6 Flash Low when the suffix and --effort disagree).
// Only agy; every other backend gets no effort key.
func AgyEffort(backend, model string) string {
	if backend != "agy" {
		return ""
	}
	for _, e := range []string{"high", "medium", "low"} {
		if strings.HasSuffix(model, "-"+e) {
			return e
		}
	}
	return ""
}
