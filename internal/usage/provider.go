package usage

import "strings"

// ProviderOf maps a (cli, model) pair to the provider whose quota it draws on.
//
// A port of hive_provider_of (hive-lib.sh), rule for rule and in the same
// order, because rotation's shadow output is diffed against the bash job and a
// different mapping would show up as phantom disagreements:
//
//  1. by CLI: copilot → github, muse → meta
//  2. by model: deepseek → deepseek; claude|opus|sonnet|haiku|fable →
//     anthropic; gpt-|codex → openai; gemini → google
//  3. by CLI again: claude|litellm → anthropic, codex → openai, agy → google,
//     bob → ibm, pi|goose → deepseek
//
// ccusage source names are accepted as CLIs too (antigravity ≡ agy), and pi's
// "[pi] " model prefix is stripped. The model rules win over the CLI because pi
// and goose front several providers.
func ProviderOf(cli, model string) string {
	c := strings.ToLower(strings.TrimSpace(cli))
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimSpace(strings.TrimPrefix(m, "[pi]"))
	switch c {
	case "copilot":
		return "github"
	case "muse":
		return "meta"
	}
	switch {
	case strings.Contains(m, "deepseek"):
		return "deepseek"
	case containsAny(m, "claude", "opus", "sonnet", "haiku", "fable"):
		return "anthropic"
	case containsAny(m, "gpt-", "codex"):
		return "openai"
	case strings.Contains(m, "gemini"):
		return "google"
	}
	switch c {
	case "claude", "litellm":
		return "anthropic"
	case "codex":
		return "openai"
	case "agy", "antigravity", "gemini":
		return "google"
	case "bob":
		return "ibm"
	case "pi", "goose":
		return "deepseek"
	}
	return "unknown"
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
