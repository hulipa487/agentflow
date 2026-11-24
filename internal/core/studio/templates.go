package studio

import (
	_ "embed"
	"fmt"
	"strings"

	"agentflow/internal/vm"
)

var (
	//go:embed templates/qq-group-bot.ts
	qqGroupBotLoop string
	//go:embed templates/qq-group-bot.md
	qqGroupBotPrompt string
	//go:embed templates/telegram-group-bot.ts
	telegramGroupBotLoop string
	//go:embed templates/telegram-group-bot.md
	telegramGroupBotPrompt string
)

// TemplateSetting describes one starter configuration field surfaced in
// Studio's Builder UI and stored inside the agent's extras map.
type TemplateSetting struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"` // bool | number | string | select
	Default     any      `json:"default"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// Template is a starter loop the builder offers. Every template carries the
// current prelude version, so a loop created from one loads without an edit.
type Template struct {
	Name         string            `json:"name"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Skills       []string          `json:"skills,omitempty"`
	Memory       string            `json:"memory,omitempty"`
	MemoryInline map[string]any    `json:"memory_inline,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	Extras       map[string]any    `json:"extras,omitempty"`
	Settings     []TemplateSetting `json:"settings,omitempty"`
	Loop         string            `json:"loop"`
}

func withCurrentPrelude(src string) string {
	norm := strings.ReplaceAll(src, "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	want := fmt.Sprintf("// af-prelude-version: %d", vm.PreludeVersion)
	lines := strings.Split(norm, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "// af-prelude-version:") {
			lines[i] = want
			return strings.Join(lines, "\n")
		}
		break
	}
	return want + "\n" + norm
}

func ftsMemoryProfile() map[string]any {
	return map[string]any{
		"stores": map[string]any{
			"dialogue": map[string]any{
				"backend":   "main_db",
				"table":     "dialogue",
				"retention": "30d",
				"window":    1000,
				"requires":  []string{"kv", "text_search"},
				"scope":     "agent",
			},
			"facts": map[string]any{
				"backend":   "main_db",
				"table":     "facts",
				"retention": "forever",
				"requires":  []string{"kv", "text_search"},
				"scope":     "agent",
			},
		},
		"write":  []string{"plugin:routing_table"},
		"recall": "plugin:recency",
	}
}

func vecMemoryProfile() map[string]any {
	return map[string]any{
		"stores": map[string]any{
			"dialogue": map[string]any{
				"backend":   "main_db",
				"table":     "dialogue",
				"retention": "30d",
				"window":    1000,
				"requires":  []string{"kv", "vector"},
				"scope":     "agent",
			},
		},
		"write":       []string{"plugin:routing_table"},
		"recall":      "plugin:semantic",
		"embed_model": "bge-m3",
	}
}

func sharedExtras(recallK int) map[string]any {
	return map[string]any{
		"bot_names":                   "御坂,yuban,御坂さん",
		"cooldown_sec":                0,
		"max_tool_rounds":             3,
		"proactive_cooldown_sec":      0,
		"proactive_eval_cooldown_sec": 0,
		"proactive_hourly_max":        64,
		"proactive_min_chars":         4,
		"proactive_wait_turns":        0,
		"recall_k":                    recallK,
		"reply_on_relevant":           true,
		"respond_on_mention":          true,
		"respond_on_reply":            true,
		"retention_turns":             20,
		"store_all_messages":          true,
		"timezone_offset_hours":       8,
		"trigger_phrase":              "",
	}
}

func qqExtras(recallK int) map[string]any {
	m := sharedExtras(recallK)
	m["respond_in_c2c"] = true
	m["respond_in_guild"] = false
	return m
}

func telegramExtras(recallK int) map[string]any {
	m := sharedExtras(recallK)
	m["respond_in_dm"] = true
	return m
}

func f64(v float64) *float64 { return &v }

func sharedSettings(recallK int, groupLabel string) []TemplateSetting {
	return []TemplateSetting{
		{
			Key:         "bot_names",
			Label:       "Bot names / aliases",
			Description: "Comma-separated names or nicknames that wake the bot in plain text without @ (used together with its bot username).",
			Type:        "string",
			Default:     "御坂,yuban,御坂さん",
			Placeholder: "御坂,yuban,御坂さん",
		},
		{
			Key:         "trigger_phrase",
			Label:       "Trigger phrase",
			Description: "Optional case-insensitive keyword or prefix (for example !bot) that wakes the bot without an @-mention.",
			Type:        "string",
			Default:     "",
			Placeholder: "!bot",
		},
		{
			Key:         "reply_on_relevant",
			Label:       "Reply to relevant group messages",
			Description: "Let the bot evaluate non-@ messages against recent group context and stay silent ([NO_REPLY]) when not relevant.",
			Type:        "bool",
			Default:     true,
		},
		{
			Key:         "cooldown_sec",
			Label:       "Group cooldown (seconds)",
			Description: "Minimum gap between direct replies in the same " + groupLabel + ". 0 disables cooldown.",
			Type:        "number",
			Default:     0,
			Min:         f64(0),
			Max:         f64(3600),
		},
		{
			Key:         "proactive_cooldown_sec",
			Label:       "Proactive reply cooldown (seconds)",
			Description: "Minimum gap between unprompted passive replies in the same group. 0 disables.",
			Type:        "number",
			Default:     0,
			Min:         f64(0),
			Max:         f64(3600),
		},
		{
			Key:         "proactive_eval_cooldown_sec",
			Label:       "Proactive eval cooldown (seconds)",
			Description: "Minimum gap between passive relevance checks in the same group. 0 disables.",
			Type:        "number",
			Default:     0,
			Min:         f64(0),
			Max:         f64(3600),
		},
		{
			Key:         "proactive_min_chars",
			Label:       "Proactive minimum message length",
			Description: "Ignore plain non-@ messages shorter than this many characters during passive checks.",
			Type:        "number",
			Default:     4,
			Min:         f64(0),
			Max:         f64(200),
		},
		{
			Key:         "proactive_wait_turns",
			Label:       "Turns to wait after bot reply",
			Description: "Require this many member messages after the bot spoke before attempting another proactive reply.",
			Type:        "number",
			Default:     0,
			Min:         f64(0),
			Max:         f64(100),
		},
		{
			Key:         "proactive_hourly_max",
			Label:       "Max proactive replies per hour",
			Description: "Hourly cap on unprompted passive replies per group session.",
			Type:        "number",
			Default:     64,
			Min:         f64(1),
			Max:         f64(500),
		},
		{
			Key:         "store_all_messages",
			Label:       "Remember skipped group messages",
			Description: "Retain ordinary group messages in working context and dialogue memory even when the bot stays quiet.",
			Type:        "bool",
			Default:     true,
		},
		{
			Key:         "retention_turns",
			Label:       "Context window (turns)",
			Description: "How many recent turns per " + groupLabel + " stay in working context.",
			Type:        "number",
			Default:     20,
			Min:         f64(1),
			Max:         f64(200),
		},
		{
			Key:         "recall_k",
			Label:       "Memory recall limit (recall_k)",
			Description: "Candidate count requested from dialogue memory (text search uses result limit; semantic vector search uses pre-filter top-k).",
			Type:        "number",
			Default:     recallK,
			Min:         f64(1),
			Max:         f64(200),
		},
		{
			Key:         "max_tool_rounds",
			Label:       "Max tool call rounds",
			Description: "Maximum model tool-calling iterations allowed per turn before forcing a final reply.",
			Type:        "number",
			Default:     3,
			Min:         f64(1),
			Max:         f64(8),
		},
		{
			Key:         "timezone_offset_hours",
			Label:       "Timezone offset (hours from UTC)",
			Description: "UTC hour offset formatted into the system prompt's current timestamp.",
			Type:        "number",
			Default:     8,
			Min:         f64(-12),
			Max:         f64(14),
		},
	}
}

func qqSettings(recallK int) []TemplateSetting {
	out := []TemplateSetting{
		{
			Key:         "respond_on_mention",
			Label:       "Reply on @-mention",
			Description: "Answer when a QQ group member @-mentions the bot.",
			Type:        "bool",
			Default:     true,
		},
		{
			Key:         "respond_on_reply",
			Label:       "Reply when quoted",
			Description: "Answer when a group member quotes or replies to one of the bot's messages without @-tagging it.",
			Type:        "bool",
			Default:     true,
		},
		{
			Key:         "respond_in_c2c",
			Label:       "Reply in C2C private chats",
			Description: "Answer 1-on-1 private QQ messages sent directly to the bot.",
			Type:        "bool",
			Default:     true,
		},
		{
			Key:         "respond_in_guild",
			Label:       "Reply in QQ Guild / channel DMs",
			Description: "Answer @-mentions in QQ Guild channels and guild DMs.",
			Type:        "bool",
			Default:     false,
		},
	}
	return append(out, sharedSettings(recallK, "QQ group")...)
}

func telegramSettings(recallK int) []TemplateSetting {
	out := []TemplateSetting{
		{
			Key:         "respond_on_mention",
			Label:       "Reply on @-mention",
			Description: "Answer when a group member @-mentions the bot or invokes a bot command.",
			Type:        "bool",
			Default:     true,
		},
		{
			Key:         "respond_on_reply",
			Label:       "Reply when replied to",
			Description: "Answer when a group member replies directly to one of the bot's messages.",
			Type:        "bool",
			Default:     true,
		},
		{
			Key:         "respond_in_dm",
			Label:       "Reply in direct messages",
			Description: "Answer private 1-on-1 messages sent directly to the bot.",
			Type:        "bool",
			Default:     true,
		},
	}
	return append(out, sharedSettings(recallK, "Telegram group")...)
}

// Templates returns the 4 starter chatbot templates supported by Studio.
func Templates() []Template {
	qqLoop := withCurrentPrelude(qqGroupBotLoop)
	qqPrompt := strings.ReplaceAll(strings.ReplaceAll(qqGroupBotPrompt, "\r\n", "\n"), "\r", "\n")
	tgLoop := withCurrentPrelude(telegramGroupBotLoop)
	tgPrompt := strings.ReplaceAll(strings.ReplaceAll(telegramGroupBotPrompt, "\r\n", "\n"), "\r", "\n")

	baseCaps := []string{"llm.chat", "memory", "tools", "net.http", "session.state"}
	qqSkills := []string{"builtin:fetch", "builtin:web_search"}
	tgSkills := []string{"builtin:fetch", "builtin:web_search", "builtin:browser"}

	return []Template{
		{
			Name:         "qq-group-bot-fts",
			Title:        "QQ Group Chatbot (FTS5)",
			Description:  "QQ group chatbot with SQLite FTS5 keyword recall, shared per-group context, proactive [NO_REPLY] filtering, Doubao search, and page fetch.",
			Capabilities: append([]string(nil), baseCaps...),
			Skills:       append([]string(nil), qqSkills...),
			MemoryInline: ftsMemoryProfile(),
			Instructions: qqPrompt,
			Extras:       qqExtras(24),
			Settings:     qqSettings(24),
			Loop:         qqLoop,
		},
		{
			Name:         "qq-group-bot-vec",
			Title:        "QQ Group Chatbot (Embedding)",
			Description:  "QQ group chatbot with bge-m3 vector semantic recall, shared per-group context, proactive [NO_REPLY] filtering, Doubao search, and page fetch.",
			Capabilities: append([]string(nil), baseCaps...),
			Skills:       append([]string(nil), qqSkills...),
			MemoryInline: vecMemoryProfile(),
			Instructions: qqPrompt,
			Extras:       qqExtras(64),
			Settings:     qqSettings(64),
			Loop:         qqLoop,
		},
		{
			Name:         "telegram-group-bot-fts",
			Title:        "Telegram Group Chatbot (FTS5)",
			Description:  "Telegram group chatbot with SQLite FTS5 keyword recall, shared per-group context, proactive [NO_REPLY] filtering, Google/X search, fetch, and browser tools.",
			Capabilities: append([]string(nil), baseCaps...),
			Skills:       append([]string(nil), tgSkills...),
			MemoryInline: ftsMemoryProfile(),
			Instructions: tgPrompt,
			Extras:       telegramExtras(24),
			Settings:     telegramSettings(24),
			Loop:         tgLoop,
		},
		{
			Name:         "telegram-group-bot-vec",
			Title:        "Telegram Group Chatbot (Embedding)",
			Description:  "Telegram group chatbot with bge-m3 vector semantic recall, shared per-group context, proactive [NO_REPLY] filtering, Google/X search, fetch, and browser tools.",
			Capabilities: append([]string(nil), baseCaps...),
			Skills:       append([]string(nil), tgSkills...),
			MemoryInline: vecMemoryProfile(),
			Instructions: tgPrompt,
			Extras:       telegramExtras(64),
			Settings:     telegramSettings(64),
			Loop:         tgLoop,
		},
	}
}
