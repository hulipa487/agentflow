// The credential CLI: `agentflow cred set|get|list|delete <name>`, operating
// on the store path from the loaded config (-config). Values
// enter through a prompt or stdin — never argv. Engine-wide credentials are
// stored under the empty user UUID; list shows names only.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/mattn/go-isatty"

	"agentflow/internal/config"
	"agentflow/internal/core/credentials"
	"agentflow/internal/llog"
)

func cmdCred(args []string, stdout, stderr io.Writer) int {
	return credMain(args, os.Stdin, stdout, stderr)
}

// credMain runs the credential CLI and returns the process exit code. Each
// command has its own flags, so a flag that does nothing for it is an error
// rather than silently ignored.
func credMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	run := func(name string) func([]string, io.Writer, io.Writer) int {
		return func(a []string, o, e io.Writer) int { return credCommand(name, a, stdin, o, e) }
	}
	return runGroup("cred", []subcommand{
		{"set", "store a credential; the value comes from a prompt or stdin, never argv", run("set")},
		{"get", "print a credential's value", run("get")},
		{"list", "list credential names (never values)", run("list")},
		{"delete", "remove a credential", run("delete")},
	}, args, stdout, stderr)
}

func credCommand(cmd string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	info := cmdInfo{Name: "cred " + cmd, Args: "NAME"}
	const keyNote = "The master key is read from the env var named by runtime.credentials.master_key_env\n(default CREDENTIALS_MASTER_KEY)."
	switch cmd {
	case "set":
		info.Summary = "Store a credential. The value is read from a prompt, or from stdin when piped; never from argv."
		info.Detail = "example: printf %s \"$TOKEN\" | agentflow cred set github_token -config deploy/\n\n" + keyNote
	case "get":
		info.Summary = "Print a credential's value."
		info.Detail = keyNote
	case "list":
		info.Args = ""
		info.Summary = "List credential names. Values are never printed."
		info.Detail = keyNote
	case "delete":
		info.Summary = "Remove a credential."
		info.Detail = keyNote
	}
	fs := newCommand(info, stderr)
	cfgPath := addConfigFlag(fs)
	user := fs.String("user", "", "a profile's credential (profile uuid, u_...); default: engine-wide")
	var kind, header, scheme *string
	if cmd == "set" {
		kind = fs.String("kind", "api", "credential kind")
		header = fs.String("header", "Authorization", "request header the secret is sent in")
		scheme = fs.String("scheme", "Bearer", "header scheme prefix")
	}
	asJSON := new(bool)
	if cmd == "get" || cmd == "list" {
		asJSON = fs.Bool("json", false, "print as JSON")
	}
	if code, ok := parseArgs(fs, args, map[string]string{"format": "use -json"}); !ok {
		return code
	}
	want := 1
	if cmd == "list" {
		want = 0
	}
	if !positionals(fs, info, want) {
		return 2
	}
	name := fs.Arg(0)
	if *user != "" && !strings.HasPrefix(*user, "u_") {
		// The identity store may not be enabled; a uuid is still a valid key.
		fmt.Fprintln(stderr, "agentflow cred: -user must be a profile uuid (u_...)")
		return 2
	}

	lvl, _ := llog.ParseLevel("error") // CLI: quiet by default, errors only
	log := slog.New(llog.NewTextHandler(stderr, lvl))

	src, err := config.ResolveSource(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "agentflow cred:", err)
		return 1
	}
	cfg, err := src.Load(log)
	if err != nil {
		fmt.Fprintln(stderr, "agentflow cred:", err)
		return 1
	}

	envName := cfg.CredentialsMasterKeyEnv()
	masterKey := os.Getenv(envName)
	if masterKey == "" {
		fmt.Fprintf(stderr, "agentflow cred: master key env var %s is not set\n", envName)
		return 1
	}
	store, err := credentials.Open(cfg.CredentialsStore(), masterKey, log)
	if err != nil {
		fmt.Fprintln(stderr, "agentflow cred:", err)
		return 1
	}
	defer store.Close()
	ctx := context.Background()

	switch cmd {
	case "set":
		value, err := readCredentialValue(name, stdin, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "agentflow cred:", err)
			return 1
		}
		if err := store.Put(ctx, *user, name, *kind, value, *header, *scheme); err != nil {
			fmt.Fprintln(stderr, "agentflow cred:", err)
			return 1
		}
		fmt.Fprintf(stdout, "stored %s (user %q)\n", name, *user)

	case "get":
		sec, ok, err := store.Get(ctx, *user, name)
		if err != nil {
			fmt.Fprintln(stderr, "agentflow cred:", err)
			return 1
		}
		if !ok {
			fmt.Fprintf(stderr, "agentflow cred: no credential named %s\n", name)
			return 1
		}
		if *asJSON {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"service": name, "user": *user, "value": sec.Value,
				"header": sec.Header, "scheme": sec.Scheme,
			})
		} else {
			fmt.Fprintln(stdout, sec.Value)
		}

	case "list":
		refs, err := store.List(ctx, *user)
		if err != nil {
			fmt.Fprintln(stderr, "agentflow cred:", err)
			return 1
		}
		if *asJSON {
			_ = json.NewEncoder(stdout).Encode(refs)
			return 0
		}
		for _, r := range refs {
			fmt.Fprintln(stdout, r.Service) // names only, never values
		}

	case "delete":
		if err := store.Delete(ctx, *user, name); err != nil {
			fmt.Fprintln(stderr, "agentflow cred:", err)
			return 1
		}
		fmt.Fprintf(stdout, "deleted %s (user %q)\n", name, *user)
	}
	return 0
}

// readCredentialValue reads the secret from stdin: an interactive prompt when
// stdin is a terminal, otherwise the piped input (minus one trailing newline).
// The value never passes through argv or any log.
func readCredentialValue(name string, stdin io.Reader, stderr io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && isatty.IsTerminal(f.Fd()) {
		fmt.Fprintf(stderr, "Enter value for %s (input is echoed; prefer piping it): ", name)
	}
	sc := bufio.NewScanner(stdin)
	// A PEM or service-account JSON runs well past the Scanner's 64 KiB default,
	// and the default reports that as Scan() returning false — which surfaced as
	// the misleading "no value on stdin for X" rather than "the value is too
	// long". One megabyte is far past any credential and still a bound.
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", fmt.Errorf("reading the value for %s: %w", name, err)
		}
		return "", fmt.Errorf("no value on stdin for %s", name)
	}
	value := sc.Text()
	// One trailing newline is a pipe artifact; strip exactly that.
	value = strings.TrimSuffix(value, "\n")
	value = strings.TrimSuffix(value, "\r")
	if value == "" {
		return "", fmt.Errorf("empty value for %s", name)
	}
	return value, nil
}
