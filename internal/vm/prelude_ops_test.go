package vm

import (
	"encoding/json"
	"reflect"
	"testing"
)

func opFromScript(t *testing.T, expr string) (Status, string) {
	t.Helper()
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	src := directive() + "function loop() { return " + expr + "; }\n"
	return st.Start("loop", "test chunk", src)
}

func TestShellSpawnForwardsCheckoutAndScratch(t *testing.T) {
	status, msg := opFromScript(t, `shell.spawn({
  image: "alpine:3.20",
  provider: "docker",
  project: "proj",
  ref: "main",
  volumes: ["data:/work"],
  scratch_mount: "/scratch",
})`)
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
	if op.Type != "shell.spawn" || op.Project != "proj" || op.Ref != "main" ||
		len(op.Volumes) != 1 || op.Volumes[0] != "data:/work" ||
		op.Scratch != "/scratch" || op.Image != "alpine:3.20" || op.Provider != "docker" {
		t.Fatalf("unexpected shell.spawn payload: %s", msg)
	}
}

func TestShellJobBuildersForwardTheirOptions(t *testing.T) {
	for _, kind := range []string{"run", "start"} {
		status, msg := opFromScript(t, `shell.`+kind+`("make test", {
  project: "site", environment: "python", workdir: "app",
  env: { CI: "1" }, timeout: 90 })`)
		if status != Yielded {
			t.Fatalf("shell.%s: expected a yielded op request, got status %v: %s", kind, status, msg)
		}
		var op struct {
			Type        string            `json:"type"`
			Cmd         string            `json:"cmd"`
			Project     string            `json:"project"`
			Environment string            `json:"environment"`
			WorkDir     string            `json:"workdir"`
			Env         map[string]string `json:"env"`
			Timeout     float64           `json:"timeout"`
		}
		if err := json.Unmarshal([]byte(msg), &op); err != nil {
			t.Fatalf("op payload is not JSON (%q): %v", msg, err)
		}
		if op.Type != "shell."+kind || op.Cmd != "make test" ||
			op.Project != "site" || op.Environment != "python" ||
			op.WorkDir != "app" || op.Env["CI"] != "1" || op.Timeout != 90 {
			t.Fatalf("shell.%s emitted %s", kind, msg)
		}
	}

	status, msg := opFromScript(t, `shell.exec("h", "ls", { timeout: 30 })`)
	var exec struct {
		Handle  string  `json:"shell_handle"`
		Timeout float64 `json:"timeout"`
	}
	if status != Yielded || json.Unmarshal([]byte(msg), &exec) != nil || exec.Handle != "h" || exec.Timeout != 30 {
		t.Fatalf("shell.exec dropped its timeout: %v %s", status, msg)
	}
	status, msg = opFromScript(t, `shell.status("j42")`)
	var st struct {
		JobID string `json:"job_id"`
	}
	if status != Yielded || json.Unmarshal([]byte(msg), &st) != nil || st.JobID != "j42" {
		t.Fatalf("shell.status dropped the job id: %v %s", status, msg)
	}
}

var builderTypes = []struct{ name, call, op string }{
	{"session.inbox", `session.inbox()`, "inbox"},
	{"session.send", `session.send("hi")`, "send"},
	{"session.push", `session.push("telegram", "1", "hi")`, "session.push"},
	{"session.push_user", `session.push_user("u_1", "hi")`, "session.push_user"},
	{"session.skip", `session.skip("cooldown")`, "session.skip"},
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
	{"store.query", `store.query("t", { kind: "all" })`, "store.query"},
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
	{"shell.run", `shell.run("ls")`, "shell.run"},
	{"shell.start", `shell.start("make")`, "shell.start"},
	{"shell.status", `shell.status("j1")`, "shell.status"},
	{"shell.cancel", `shell.cancel("j1")`, "shell.cancel"},
	{"ssh.spawn", `ssh.spawn({})`, "ssh.spawn"},
	{"ssh.exec", `ssh.exec("h", "ls")`, "ssh.exec"},
	{"ssh.read", `ssh.read("h", "f")`, "ssh.read"},
	{"ssh.write", `ssh.write("h", "f", "c")`, "ssh.write"},
	{"ssh.close", `ssh.close("h")`, "ssh.close"},
	{"http.request", `http.request({ url: "https://x" })`, "http.request"},
	{"http.get", `http.get("https://x")`, "http.request"},
	{"http.post", `http.post("https://x", "b")`, "http.request"},
	{"os.env", `os.env("X")`, "os.env"},
	{"runtime.triggers", `runtime.triggers()`, "runtime.triggers"},
	{"credential.get", `credential.get("github")`, "credential.get"},
	{"mail.imap_fetch", `mail.imap_fetch({})`, "mail.imap.fetch"},
	{"mail.smtp_send", `mail.smtp_send({})`, "mail.smtp.send"},
	{"llm.chat", `llm.chat([])`, "llm.chat"},
	{"llm.embed", `llm.embed("x")`, "llm.embed"},
	{"llm.rerank", `llm.rerank("q", ["a"])`, "llm.rerank"},
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
	{"api.list", `api.list()`, "api.list"},
	{"api.call", `api.call("github", "list_repos", {})`, "api.call"},
	{"api.batch", `api.batch([{ api: "github", action: "list_repos", args: {} }])`, "api.batch"},
	{"api proxy", `api.github.list_repos({ owner: "acme" })`, "api.call"},
}

func TestEveryBuilderEmitsItsOpType(t *testing.T) {
	for _, c := range builderTypes {
		t.Run(c.name, func(t *testing.T) {
			status, msg := opFromScript(t, c.call)
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

func TestOpBuildersForwardTheirOptions(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want map[string]any
	}{
		{
			name: "session.skip carries reason",
			expr: `session.skip("cooldown")`,
			want: map[string]any{
				"type": "session.skip",
				"text": "cooldown",
			},
		},
		{
			name: "http.request carries query_params and save_to",
			expr: `http.request({ url: "https://x", query: { city: "Tokyo" }, save_to: "body.txt", timeout: 5 })`,
			want: map[string]any{
				"type":         "http.request",
				"query_params": map[string]any{"city": "Tokyo"},
				"save_to":      "body.txt",
				"timeout":      5.0,
			},
		},
		{
			name: "store.put carries ttl and vector",
			expr: `store.put("t", "k", { a: 1 }, { ttl: 30, vector: [0.5, 0.25] })`,
			want: map[string]any{
				"type": "store.put", "table": "t", "key": "k",
				"ttl": 30.0, "vector": []any{0.5, 0.25},
			},
		},
		{
			name: "store.query injects the table",
			expr: `store.query("t", { kind: "prefix", prefix: "a" })`,
			want: map[string]any{
				"type":  "store.query",
				"query": map[string]any{"kind": "prefix", "prefix": "a", "table": "t"},
			},
		},
		{
			name: "llm.chat passes its options through",
			expr: `llm.chat([{ role: "user", content: "hi" }], { model: "m", temperature: 0.2, max_tokens: 64, thinking: "low", tool_choice: "auto" })`,
			want: map[string]any{
				"type": "llm.chat", "model": "m", "temperature": 0.2,
				"max_tokens": 64.0, "thinking": "low", "tool_choice": "auto",
			},
		},
		{
			name: "llm.embed passes its options through",
			expr: `llm.embed(["a", "b"], { model: "m", task: "retrieval.query", dimensions: 512, merged: true })`,
			want: map[string]any{
				"type": "llm.embed", "model": "m", "task": "retrieval.query",
				"dimensions": 512.0, "merged": true,
			},
		},
		{
			name: "llm.rerank carries top_n",
			expr: `llm.rerank("q", ["a", "b"], { model: "m", top_n: 1 })`,
			want: map[string]any{"type": "llm.rerank", "top_n": 1.0, "model": "m"},
		},
		{
			name: "agent.request carries its timeout",
			expr: `agent.request("agent:other", { go: true }, 15)`,
			want: map[string]any{
				"type": "agent.request", "address": "agent:other",
				"timeout": 15.0, "payload": map[string]any{"go": true},
			},
		},
		{
			name: "scheduler.every carries its interval",
			expr: `scheduler.every(30)`,
			want: map[string]any{"type": "scheduler.every", "interval": 30.0},
		},
		{
			name: "scheduler.after carries its delay",
			expr: `scheduler.after(5)`,
			want: map[string]any{"type": "scheduler.after", "delay": 5.0},
		},
		{
			name: "session.push carries channel, reply_to and attachments",
			expr: `session.push("telegram", "99", "hi", { attachments: [{ type: "image", mime: "image/png", handle: "media:ab" }] })`,
			want: map[string]any{
				"type": "session.push", "channel": "telegram", "reply_to": "99", "text": "hi",
				"attachments": []any{map[string]any{"type": "image", "mime": "image/png", "handle": "media:ab"}},
			},
		},
		{
			name: "tools.run ignores script confirmed flags and forwards idempotency_key",
			expr: `tools.run("builtin:x", { a: 1 }, { confirmed: true, confirm_id: "c1", idempotency_key: "k1" })`,
			want: map[string]any{"type": "tools.run", "idempotency_key": "k1"},
		},
		{
			name: "files.put carries content and mime",
			expr: `files.put("p", "a.txt", "hi", { mime: "text/plain" })`,
			want: map[string]any{
				"type": "files.put", "project": "p", "path": "a.txt",
				"content": "hi", "mime": "text/plain",
			},
		},
		{
			name: "files.put accepts a base64 object",
			expr: `files.put("p", "a.bin", { data: "AAEC", mime: "application/octet-stream" })`,
			want: map[string]any{
				"type": "files.put", "data": "AAEC", "mime": "application/octet-stream",
			},
		},
		{
			name: "files.commit carries ref and message",
			expr: `files.commit("p", { ref: "dev", message: "wip" })`,
			want: map[string]any{"type": "files.commit", "ref": "dev", "commit_msg": "wip"},
		},
		{
			name: "mail.imap_fetch carries its connection fields",
			expr: `mail.imap_fetch({ host: "h", port: 993, user: "u", mailbox: "INBOX", unseen: true, limit: 5 })`,
			want: map[string]any{
				"type": "mail.imap.fetch", "mail_host": "h", "mail_port": 993.0,
				"mail_user": "u", "mailbox": "INBOX", "unseen": true, "limit": 5.0,
			},
		},
		{
			name: "mail.smtp_send carries its message fields",
			expr: `mail.smtp_send({ host: "h", port: 587, user: "u", from: "a@x", to: ["b@y"], subject: "s", text_body: "t" })`,
			want: map[string]any{
				"type": "mail.smtp.send", "mail_host": "h", "mail_port": 587.0,
				"mail_from": "a@x", "mail_to": []any{"b@y"}, "subject": "s", "text_body": "t",
			},
		},
		{
			name: "api proxy forwards pkg, action and args",
			expr: `api.slack.post_message({ channel: "#general", text: "hi" })`,
			want: map[string]any{
				"type": "api.call", "api": "slack", "action": "post_message",
				"args": map[string]any{"channel": "#general", "text": "hi"},
			},
		},
		{
			name: "api.batch forwards normalized calls",
			expr: `api.batch([{ api: "a", action: "x", args: { n: 1 } }, { api: "b", action: "y" }])`,
			want: map[string]any{
				"type": "api.batch",
				"calls": []any{
					map[string]any{"api": "a", "action": "x", "args": map[string]any{"n": 1.0}},
					map[string]any{"api": "b", "action": "y", "args": map[string]any{}},
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, msg := opFromScript(t, c.expr)
			if status != Yielded {
				t.Fatalf("%s did not yield an op: status %v, %s", c.expr, status, msg)
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
			if _, ok := got["confirmed"]; ok {
				t.Errorf("prelude leaked forged confirmed flag: %s", msg)
			}
			if _, ok := got["confirm_id"]; ok {
				t.Errorf("prelude leaked forged confirm_id: %s", msg)
			}
		})
	}
}
