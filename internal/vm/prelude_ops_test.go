package vm

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The Lua-facing op builders are the only way a loop reaches these features, so
// a field one of them drops is a feature no loop can reach — and silently, when
// the Go handler reads an empty value and does the plain thing.
//
// Two bugs of exactly that shape have shipped. shell.spawn did not forward the
// engine's checkout (project + ref) or scratch_mount, so a loop asking for a
// checkout got a plain container and no error. http.request emitted its query
// table under the key "query" while session.Op reads `json:"query_params"`, so
// the parameter was accepted, decoded to the zero value, and ignored — and
// save_to had no Lua binding at all, though the op and the Go handler both
// support it and builtin:fetch documents it.
//
// Both survived because nothing compared a Lua-side key name against the
// Go-side wire name. These tests do.
//
// Adding a builder: add a row to both tables. That is the whole point.
func TestShellSpawnForwardsCheckoutAndScratch(t *testing.T) {
	status, msg := opFromLua(t, `
local h = shell.spawn({ image = "alpine:3.20", provider = "docker",
                        project = "proj", ref = "main",
                        volumes = { "data:/work" }, scratch_mount = "/scratch" })
return h
`)
	if status != Yielded {
		t.Fatalf("expected a yielded op request, got status %v: %s", status, msg)
	}

	var op struct {
		Type     string   `json:"type"`
		Image    string   `json:"image"`
		Project  string   `json:"project"`
		Ref      string   `json:"ref"`
		Volumes  []string `json:"volumes"`
		Scratch  string   `json:"scratch_mount"`
		Provider string   `json:"provider"`
	}
	if err := json.Unmarshal([]byte(msg), &op); err != nil {
		t.Fatalf("op payload is not JSON (%q): %v", msg, err)
	}
	if op.Type != "shell.spawn" {
		t.Fatalf("op type = %q", op.Type)
	}
	if op.Project != "proj" || op.Ref != "main" {
		t.Fatalf("the spawn carried no checkout: %s", msg)
	}
	if len(op.Volumes) != 1 || op.Volumes[0] != "data:/work" {
		t.Fatalf("the spawn carried no volumes: %s", msg)
	}
	if op.Scratch != "/scratch" {
		t.Fatalf("the spawn carried no scratch mount: %s", msg)
	}
	if op.Image != "alpine:3.20" || op.Provider != "docker" {
		t.Fatalf("the plain fields stopped being forwarded: %s", msg)
	}
}

// opFromLua starts a chunk that calls one builder, and returns the op JSON it
// yielded.
func opFromLua(t *testing.T, body string) (Status, string) {
	t.Helper()
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	return st.Start("loop", body)
}

// builderTypes is every builder that yields an op directly, with the op type it
// must produce. A renamed or deleted builder fails here rather than at a loop's
// first call — which is how a builder that no longer exists stops being
// discoverable only by reading the prelude.
//
// memory.write/recall are absent on purpose: they compose over the store ops
// through a support chunk the base prelude does not load.
var builderTypes = []struct{ name, call, op string }{
	{"session.inbox", `session.inbox()`, "inbox"},
	{"session.send", `session.send("hi")`, "send"},
	{"session.push", `session.push("telegram", "1", "hi")`, "session.push"},
	{"session.push_user", `session.push_user("u_1", "hi")`, "session.push_user"},
	{"session.exit", `session.exit()`, "session.exit"},
	{"session.state.set", `session.state.set("k", 1)`, "session.state.set"},
	{"session.state.get", `session.state.get("k")`, "session.state.get"},
	{"session.state.delete", `session.state.delete("k")`, "session.state.delete"},
	{"session.state.list", `session.state.list()`, "session.state.list"},
	{"time.sleep", `time.sleep(1)`, "sleep"},
	{"log.info", `log.info("x")`, "log"},
	{"agent.info", `agent.info()`, "agent.info"},
	{"agent.config", `agent.config()`, "agent.config"},
	{"agent.send", `agent.send("agent:a", {})`, "agent.send"},
	{"agent.request", `agent.request("agent:a")`, "agent.request"},
	{"agent.reply", `agent.reply("r", {})`, "agent.reply"},
	{"agent.list", `agent.list()`, "agent.list"},
	{"agent.spawn", `agent.spawn("p", {})`, "agent.spawn"},
	{"scheduler.every", `scheduler.every(30)`, "scheduler.every"},
	{"scheduler.after", `scheduler.after(5)`, "scheduler.after"},
	{"scheduler.cron", `scheduler.cron("*/5 * * * *")`, "scheduler.cron"},
	{"scheduler.cancel", `scheduler.cancel("t")`, "scheduler.cancel"},
	{"store.put", `store.put("t", "k", 1)`, "store.put"},
	{"store.get", `store.get("t", "k")`, "store.get"},
	{"store.query", `store.query("t", { kind = "all" })`, "store.query"},
	{"store.delete", `store.delete("t", "k")`, "store.delete"},
	{"store.scopes", `store.scopes("t")`, "store.scopes"},
	{"user.current", `user.current()`, "user.current"},
	{"user.usage", `user.usage()`, "user.usage"},
	{"user.has_credential", `user.has_credential("github")`, "user.has_credential"},
	{"user.get", `user.get("u_1")`, "user.get"},
	{"user.list", `user.list()`, "user.list"},
	{"tools.list", `tools.list()`, "tools.list"},
	{"tools.run", `tools.run("x", {})`, "tools.run"},
	{"shell.spawn", `shell.spawn({})`, "shell.spawn"},
	{"shell.exec", `shell.exec("h", "ls")`, "shell.exec"},
	{"shell.write", `shell.write("h", "f", "c")`, "shell.write"},
	{"shell.destroy", `shell.destroy("h")`, "shell.destroy"},
	{"http.request", `http.request({ url = "https://x" })`, "http.request"},
	{"http.get", `http.get("https://x")`, "http.request"},
	{"http.post", `http.post("https://x", "b")`, "http.request"},
	{"os.env", `os.env("X")`, "os.env"},
	{"runtime.triggers", `runtime.triggers()`, "runtime.triggers"},
	{"credential.get", `credential.get("github")`, "credential.get"},
	{"mail.imap_fetch", `mail.imap_fetch({})`, "mail.imap.fetch"},
	{"mail.smtp_send", `mail.smtp_send({})`, "mail.smtp.send"},
	{"llm.chat", `llm.chat({})`, "llm.chat"},
	{"llm.embed", `llm.embed("x")`, "llm.embed"},
	{"llm.rerank", `llm.rerank("q", { "a" })`, "llm.rerank"},
	{"files.put", `files.put("p", "a", "x")`, "files.put"},
	{"files.read", `files.read("p", "a")`, "files.read"},
	{"files.list", `files.list("p")`, "files.list"},
	{"files.projects", `files.projects()`, "files.projects"},
	{"files.delete", `files.delete("p", "a")`, "files.delete"},
	{"files.commit", `files.commit("p")`, "files.commit"},
	{"files.checkout", `files.checkout("p", "main")`, "files.checkout"},
	{"files.scratch.put", `files.scratch.put("n", "x")`, "files.scratch.put"},
	{"files.scratch.read", `files.scratch.read("n")`, "files.scratch.read"},
	{"files.scratch.list", `files.scratch.list()`, "files.scratch.list"},
}

func TestEveryBuilderEmitsItsOpType(t *testing.T) {
	for _, c := range builderTypes {
		t.Run(c.name, func(t *testing.T) {
			status, msg := opFromLua(t, "return "+c.call)
			if status != Yielded {
				t.Fatalf("%s did not yield an op: status %v, %s", c.call, status, msg)
			}
			var op struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(msg), &op); err != nil {
				t.Fatalf("op payload is not JSON (%q): %v", msg, err)
			}
			if op.Type != c.op {
				t.Errorf("%s emitted op type %q; want %q", c.call, op.Type, c.op)
			}
		})
	}
}

// TestOpBuildersForwardTheirOptions asserts the wire key for the options a
// builder accepts. A key that does not match session.Op's json tag is dropped
// without an error, which is precisely how http.request lost its query table:
// both sides compiled, and the option was simply unreachable.
func TestOpBuildersForwardTheirOptions(t *testing.T) {
	cases := []struct {
		name string
		lua  string
		want map[string]any
	}{
		{
			name: "http.request carries query_params and save_to",
			lua:  `http.request({ url = "https://x", query = { city = "Tokyo" }, save_to = "body.txt", timeout = 5 })`,
			want: map[string]any{
				"type":         "http.request",
				"query_params": map[string]any{"city": "Tokyo"},
				"save_to":      "body.txt",
				"timeout":      5.0,
			},
		},
		{
			name: "store.put carries ttl and vector",
			lua:  `store.put("t", "k", { a = 1 }, { ttl = 30, vector = { 0.5, 0.25 } })`,
			want: map[string]any{
				"type": "store.put", "table": "t", "key": "k",
				"ttl": 30.0, "vector": []any{0.5, 0.25},
			},
		},
		{
			name: "store.query injects the table",
			lua:  `store.query("t", { kind = "prefix", prefix = "a" })`,
			want: map[string]any{
				"type":  "store.query",
				"query": map[string]any{"kind": "prefix", "prefix": "a", "table": "t"},
			},
		},
		{
			// llm.chat spreads its opts table by name, so this pins the
			// vocabulary a loop actually uses (react.lua depends on model,
			// tools and tool_choice).
			name: "llm.chat passes its options through",
			lua:  `llm.chat({ { role = "user", content = "hi" } }, { model = "m", temperature = 0.2, max_tokens = 64, thinking = "low", tool_choice = "auto" })`,
			want: map[string]any{
				"type": "llm.chat", "model": "m", "temperature": 0.2,
				"max_tokens": 64.0, "thinking": "low", "tool_choice": "auto",
			},
		},
		{
			name: "llm.embed passes its options through",
			lua:  `llm.embed({ "a", "b" }, { model = "m", task = "retrieval.query", dimensions = 512, merged = true })`,
			want: map[string]any{
				"type": "llm.embed", "model": "m", "task": "retrieval.query",
				"dimensions": 512.0, "merged": true,
			},
		},
		{
			name: "llm.rerank carries top_n",
			lua:  `llm.rerank("q", { "a", "b" }, { model = "m", top_n = 1 })`,
			want: map[string]any{"type": "llm.rerank", "top_n": 1.0, "model": "m"},
		},
		{
			name: "agent.request carries its timeout",
			lua:  `agent.request("agent:other", { go = true }, 15)`,
			want: map[string]any{
				"type": "agent.request", "address": "agent:other",
				"timeout": 15.0, "payload": map[string]any{"go": true},
			},
		},
		{
			name: "scheduler.every carries its interval",
			lua:  `scheduler.every(30)`,
			want: map[string]any{"type": "scheduler.every", "interval": 30.0},
		},
		{
			name: "scheduler.after carries its delay",
			lua:  `scheduler.after(5)`,
			want: map[string]any{"type": "scheduler.after", "delay": 5.0},
		},
		{
			name: "session.push carries channel, reply_to and attachments",
			lua:  `session.push("telegram", "99", "hi", { attachments = { { type = "image", mime = "image/png", handle = "media:ab" } } })`,
			want: map[string]any{
				"type": "session.push", "channel": "telegram", "reply_to": "99", "text": "hi",
				"attachments": []any{map[string]any{"type": "image", "mime": "image/png", "handle": "media:ab"}},
			},
		},
		{
			name: "tools.run carries the confirmation pair",
			lua:  `tools.run("builtin:x", { a = 1 }, { confirmed = true, confirm_id = "c1" })`,
			want: map[string]any{"type": "tools.run", "confirmed": true, "confirm_id": "c1"},
		},
		{
			name: "files.put carries content and mime",
			lua:  `files.put("p", "a.txt", "hi", { mime = "text/plain" })`,
			want: map[string]any{
				"type": "files.put", "project": "p", "path": "a.txt",
				"content": "hi", "mime": "text/plain",
			},
		},
		{
			name: "files.put accepts a base64 object",
			lua:  `files.put("p", "a.bin", { data = "AAEC", mime = "application/octet-stream" })`,
			want: map[string]any{
				"type": "files.put", "data": "AAEC", "mime": "application/octet-stream",
			},
		},
		{
			name: "files.commit carries ref and message",
			lua:  `files.commit("p", { ref = "dev", message = "wip" })`,
			want: map[string]any{"type": "files.commit", "ref": "dev", "commit_msg": "wip"},
		},
		{
			name: "mail.imap_fetch carries its connection fields",
			lua:  `mail.imap_fetch({ host = "h", port = 993, user = "u", mailbox = "INBOX", unseen = true, limit = 5 })`,
			want: map[string]any{
				"type": "mail.imap.fetch", "mail_host": "h", "mail_port": 993.0,
				"mail_user": "u", "mailbox": "INBOX", "unseen": true, "limit": 5.0,
			},
		},
		{
			name: "mail.smtp_send carries its message fields",
			lua:  `mail.smtp_send({ host = "h", port = 587, user = "u", from = "a@x", to = { "b@y" }, subject = "s", text_body = "t" })`,
			want: map[string]any{
				"type": "mail.smtp.send", "mail_host": "h", "mail_port": 587.0,
				"mail_from": "a@x", "mail_to": []any{"b@y"}, "subject": "s", "text_body": "t",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, msg := opFromLua(t, "return "+c.lua)
			if status != Yielded {
				t.Fatalf("%s did not yield an op: status %v, %s", c.lua, status, msg)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(msg), &got); err != nil {
				t.Fatalf("op payload is not JSON (%q): %v", msg, err)
			}
			for k, want := range c.want {
				if !reflect.DeepEqual(got[k], want) {
					t.Errorf("field %q = %#v; want %#v\nfull op: %s", k, got[k], want, msg)
				}
			}
		})
	}
}
