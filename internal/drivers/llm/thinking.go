package llm

import (
	"fmt"
	"strings"

	"agentflow/internal/config"

	"google.golang.org/genai/interactions/models/interactions"
)

// Thinking is the provider-neutral thinking level: one vocabulary for every
// provider, mapped to each one's native request shape at request time. The
// levels are deliberately coarse — providers disagree on what their own
// knobs mean (a token budget, an effort name), but they all order the same
// way, so a single ladder is the honest common subset.
//
// Settable per model (models.<name>.thinking) as the default, and per call
// (llm.chat opts.thinking), the call winning. Empty means "do not send
// anything" — the provider's own default behavior, whatever that is. An
// unrecognized value fails the call with a clear error rather than being
// silently dropped, because a dropped value looks like a working config.
//
// How each provider interprets a level:
//
//	anthropic        budget_tokens on a {type:"enabled"} thinking block
//	openai           reasoning_effort on chat completions
//	openai-responses reasoning.effort on the Responses API
//	gemini           generation_config.thinking_level (+ thinking_summaries)
type Thinking string

const (
	ThinkingOff    Thinking = "off"
	ThinkingLow    Thinking = "low"
	ThinkingMedium Thinking = "medium"
	ThinkingHigh   Thinking = "high"
	ThinkingXHigh  Thinking = "xhigh"
	ThinkingMax    Thinking = "max"
)

// ParseThinking validates one thinking level. The empty string is valid and
// means "unset" — providers then fall back to their own defaults.
func ParseThinking(s string) (Thinking, error) {
	t := Thinking(strings.ToLower(strings.TrimSpace(s)))
	switch t {
	case "", ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh, ThinkingMax:
		return t, nil
	}
	return "", fmt.Errorf("invalid thinking level %q (want off|low|medium|high|xhigh|max)", s)
}

// thinkingOf resolves the level for one call: the per-call override wins,
// then the per-model default, then unset. An invalid value anywhere is an
// error so a typo in config fails loudly on first use, not silently forever.
func thinkingOf(cfg config.Model, opts Opts) (Thinking, error) {
	raw := opts.Thinking
	if raw == "" {
		raw = cfg.Thinking
	}
	return ParseThinking(raw)
}

// anthropicThinkingBudget maps a level onto Anthropic's budget_tokens and
// clamps it into the request's output budget. Thinking and the answer share
// max_tokens, so a budget that leaves no room for a reply would only 400 at
// the provider; the clamp keeps the request well-formed at every level. The
// 1024 floor is Anthropic's documented minimum budget.
var anthropicThinkingBudget = map[Thinking]int{
	ThinkingLow:    4096,
	ThinkingMedium: 16384,
	ThinkingHigh:   32768,
	ThinkingXHigh:  65536,
	ThinkingMax:    131072,
}

func anthropicBudgetFor(t Thinking, maxTokens int) int {
	budget := anthropicThinkingBudget[t]
	if room := maxTokens - 1024; budget > room {
		budget = room
	}
	if budget < 1024 {
		budget = 1024
	}
	return budget
}

// openaiEffort maps a level onto OpenAI's reasoning_effort name. "off" is
// "none" (reasoning explicitly disabled), and "max" — Anthropic vocabulary —
// tops out at "high", the top of OpenAI's ladder; sending a name a model
// does not know would 400 honestly, but degrading to the real maximum is
// more useful.
var openaiEffort = map[Thinking]string{
	ThinkingOff:    "none",
	ThinkingLow:    "low",
	ThinkingMedium: "medium",
	ThinkingHigh:   "high",
	ThinkingXHigh:  "xhigh",
	ThinkingMax:    "high",
}

// geminiThinkingLevel maps a level onto the Interactions API's thinking_level.
// The ladder is four rungs wide (minimal|low|medium|high), so "off" — a level
// this provider used to express as thinking_budget 0 — bottoms out at minimal,
// the lowest thinking it can ask for, and the two Anthropic-flavoured top rungs
// both land on high. A non-off level additionally sets thinking_summaries,
// which is how the API is asked to return the thought summaries the caller
// reads back (the old include_thoughts flag).
var geminiThinkingLevel = map[Thinking]interactions.ThinkingLevel{
	ThinkingOff:    interactions.ThinkingLevelMinimal,
	ThinkingLow:    interactions.ThinkingLevelLow,
	ThinkingMedium: interactions.ThinkingLevelMedium,
	ThinkingHigh:   interactions.ThinkingLevelHigh,
	ThinkingXHigh:  interactions.ThinkingLevelHigh,
	ThinkingMax:    interactions.ThinkingLevelHigh,
}
