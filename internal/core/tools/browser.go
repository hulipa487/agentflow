package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentflow/internal/core/files"
	"agentflow/internal/drivers/browser"
)

// maxLinks caps how many links an action=links result carries. A nav-heavy
// page can hold thousands, and a tool result is handed to the model whole.
const maxLinks = 500

// maxCharsDefault bounds the text an action=markdown or action=content result
// carries when the caller does not say otherwise. A rendered page is routinely
// hundreds of KB.
//
// The bound is not a nicety. Nothing in the engine truncates a tool result:
// the value returned here is marshalled, crosses the Luau bridge, and is
// serialized into the provider request in full. Worse, the loop's token
// budgeter counts a message's `content` only, so an oversized tool result is
// invisible to it — the trimmer keeps it and evicts real history to make room.
// Reporting the untruncated length alongside the cut text is what lets a model
// tell a short page from a clipped one and page through with a narrower
// request; builtin:legal_read's start/max_chars/truncated contract is the
// same idea.
const maxCharsDefault = 20000

// RegisterBrowserBuiltins adds builtin:browser, backed by the configured
// Cloudflare Browser Run account (config.Browser). With no account configured
// it reports honest-unavailable rather than failing.
//
// One tool with an `action` enum rather than one tool per action, mirroring how
// builtin:web_search carries an `engine` enum: the actions share nearly every
// parameter, so six near-identical schemas would spend every agent's context
// on the difference between them.
func RegisterBrowserBuiltins(r *Registry, b *browser.Client, fm *files.Manager) {
	actions := browser.Actions()
	r.Register(ToolSpec{
		Name: "builtin:browser",
		Description: fmt.Sprintf(
			"Read a web page through a real headless browser, so JavaScript-rendered content is present (a plain HTTP fetch of a single-page app usually returns an empty shell). "+
				"action selects what comes back: %s. "+
				"markdown returns the page as Markdown, content the fully rendered HTML, links every link on the page, scrape extracts the elements matching `selectors`, "+
				"json extracts structured data described by `prompt` and/or `schema` (this action runs an AI model and is billed), accessibility_tree returns the accessibility tree. "+
				"For pages whose content loads after the initial document, set wait_until to networkidle2. "+
				"Long text is truncated: the result reports the true length as `chars` and sets `truncated` when it was cut. "+
				"Returns honest unavailable if no Browser Run account is configured.",
			strings.Join(actions, " | ")),
		Parameters: objectSchema(map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        actions,
				"description": "What to return for the page.",
			},
			"url":  map[string]any{"type": "string", "description": "Page to load. Required unless `html` is given."},
			"html": map[string]any{"type": "string", "description": "Raw HTML to render instead of loading a URL."},
			"wait_until": map[string]any{
				"type":        "string",
				"enum":        []string{"load", "domcontentloaded", "networkidle0", "networkidle2"},
				"description": "When the page counts as loaded (default domcontentloaded). Use networkidle2 for JavaScript-heavy pages.",
			},
			"wait_for_selector": map[string]any{
				"type":        "string",
				"description": "Wait for this CSS selector to appear before acting. Faster than wait_until when you know what you are waiting for.",
			},
			"timeout_ms": map[string]any{"type": "number", "description": "Page-load timeout in ms (max 60000)."},
			"user_agent": map[string]any{"type": "string", "description": "Override the User-Agent. Does not bypass bot protection."},
			"selectors": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "action=scrape only: CSS selectors to extract, e.g. [\"h1\", \"a\"].",
			},
			"prompt": map[string]any{"type": "string", "description": "action=json only: what to extract, in natural language."},
			"schema": map[string]any{"type": "object", "description": "action=json only: JSON Schema the extracted data must match."},
			"max_chars": map[string]any{
				"type":        "number",
				"description": fmt.Sprintf("Cap on returned text (default %d, 0 = no cap). The result reports the true length and whether it was cut.", maxCharsDefault),
			},
			"save_to": map[string]any{
				"type":        "string",
				"description": "Scratch file name: the page's text (markdown / html / accessibility tree) also lands in this session's files scratch space. Text actions only; other actions report saved.error.",
			},
		}, []string{"action"}),
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			if b.Empty() {
				return ResultUnavailable("builtin:browser", "No Browser Run account is configured."), nil
			}
			action := browser.Action(argString(args["action"]))
			url := argString(args["url"])
			html := argString(args["html"])
			selectors := argStrings(args["selectors"])
			prompt := argString(args["prompt"])
			schema, _ := args["schema"].(map[string]any)

			// Bad arguments are the caller's mistake, not a failed request, so
			// they come back as a result the model can correct from rather than
			// as a transport error — the shape builtin:shell.exec uses.
			if !browser.Implemented(action) {
				return browserArgErr(fmt.Sprintf("unknown action %q (want one of %s)", action, strings.Join(actions, ", ")))
			}
			if url == "" && html == "" {
				return browserArgErr("url or html is required")
			}
			if action == browser.ActionScrape && len(selectors) == 0 {
				return browserArgErr("action=scrape requires selectors")
			}
			if action == browser.ActionJSON && prompt == "" && len(schema) == 0 {
				return browserArgErr("action=json requires prompt or schema")
			}

			maxChars := argMaxChars(args["max_chars"])
			res, err := b.Do(ctx, browser.Request{
				Action:          action,
				URL:             url,
				HTML:            html,
				WaitUntil:       argString(args["wait_until"]),
				WaitForSelector: argString(args["wait_for_selector"]),
				TimeoutMS:       argCount(args["timeout_ms"]),
				UserAgent:       argString(args["user_agent"]),
				Selectors:       selectors,
				Prompt:          prompt,
				Schema:          schema,
			})
			if err != nil {
				return nil, fmt.Errorf("builtin:browser: %w", err)
			}

			out := map[string]any{
				"ok":           true,
				"tool":         "builtin:browser",
				"action":       string(res.Action),
				"time_cost_ms": res.TimeCostMs,
			}
			if url != "" {
				out["url"] = url
			}
			if res.Meta != nil {
				if res.Meta.Title != "" {
					out["title"] = res.Meta.Title
				}
				if res.Meta.Status != 0 {
					out["status"] = res.Meta.Status
				}
			}
			switch res.Action {
			case browser.ActionMarkdown:
				addBrowserText(out, "markdown", res.Markdown, maxChars)
			case browser.ActionContent:
				addBrowserText(out, "html", res.Content, maxChars)
			case browser.ActionLinks:
				out["count"] = len(res.Links)
				if len(res.Links) > maxLinks {
					out["links"] = res.Links[:maxLinks]
					out["truncated"] = true
				} else {
					out["links"] = res.Links
				}
			case browser.ActionScrape:
				out["elements"] = res.Elements
			case browser.ActionJSON:
				out["response"] = res.Response
			case browser.ActionAccessibilityTree:
				out["tree"] = res.Tree
			}
			if name := argString(args["save_to"]); name != "" {
				var text string
				switch res.Action {
				case browser.ActionMarkdown:
					text = res.Markdown
				case browser.ActionContent:
					text = res.Content
				case browser.ActionAccessibilityTree:
					if b, err := json.Marshal(res.Tree); err == nil {
						text = string(b)
					}
				}
				if text == "" {
					out["saved"] = map[string]any{"name": name, "error": fmt.Sprintf("action=%s has no text payload to save", res.Action)}
				} else {
					saveToScratch(ctx, fm, out, name, []byte(text), "text/plain; charset=utf-8")
				}
			}
			return out, nil
		},
	})
}

// browserArgErr reports a malformed call in the result itself, with a nil
// error, so the model sees a correctable message rather than a tool failure.
func browserArgErr(msg string) (any, error) {
	return map[string]any{"ok": false, "tool": "builtin:browser", "error": msg}, nil
}

// addBrowserText sets a text field, reporting the untruncated length and
// whether it was cut. The cut is rune-safe: slicing bytes could split a
// multi-byte character and emit invalid UTF-8 into the provider request.
func addBrowserText(out map[string]any, key, text string, max int) {
	out["chars"] = len(text)
	if max > 0 && len(text) > max {
		out[key] = strings.ToValidUTF8(text[:max], "")
		out["truncated"] = true
		return
	}
	out[key] = text
}

// argMaxChars reads max_chars under the "unset means default, 0 means no cap"
// rule, which argCount alone cannot express — it returns 0 for both. A
// negative or non-numeric value falls back to the default rather than being
// taken as "no cap".
func argMaxChars(v any) int {
	if v == nil {
		return maxCharsDefault
	}
	n := argCount(v)
	if n < 0 {
		return maxCharsDefault
	}
	return n
}

// argString reads a string arg, "" when it is absent or another type.
func argString(v any) string {
	s, _ := v.(string)
	return s
}

// argStrings reads an array arg. Lua delivers arrays as []any, so asserting
// []string alone would miss every value the model actually sends; both are
// accepted. Empty entries are dropped.
func argStrings(v any) []string {
	switch list := v.(type) {
	case []string:
		out := make([]string, 0, len(list))
		for _, s := range list {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
