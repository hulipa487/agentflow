// The credential CLI: `agentflow cred set|get|list|delete <name>`, operating
// on the store path from the loaded config (configdir or -config). Values
// enter through a prompt or stdin — never argv. Engine-wide credentials are
// stored under the empty user UUID; list shows names only.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
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

const credUsage = `usage: agentflow cred <command> [flags] <name>

commands:
  set <name>     store a credential (value from a prompt or stdin, never argv)
  get <name>     print a credential value
  list           list credential names (never values)
  delete <name>  remove a credential

flags:
  -config <path>      config file or directory (default: agentflow.yaml, then config.yaml)
  -configdir <dir>    deprecated: config directory (use -config DIR)
  -user <uuid>        store/list this profile's credential (default: engine-wide)
  -kind <kind>        credential kind for set (default "api")
  -header <name>      request header the secret is injected into (default Authorization)
  -scheme <scheme>    header scheme prefix (default Bearer)
  -format text|json   output format for get/list (default text)

the master key is read from the env var named by
runtime.credentials.master_key_env (default CREDENTIALS_MASTER_KEY).

examples:
  printf %s "$TOKEN" | agentflow cred set github_token -config deploy/system.yaml
  agentflow cred get github_token
  agentflow cred list
`

// credMain runs the credential CLI and returns the process exit code.
func credMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cred", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file or directory")
	configDir := fs.String("configdir", "", "deprecated: config directory (use -config DIR)")
	user := fs.String("user", "", "profile uuid (default: engine-wide)")
	kind := fs.String("kind", "api", "credential kind")
	header := fs.String("header", "Authorization", "request header")
	scheme := fs.String("scheme", "Bearer", "header scheme")
	format := fs.String("format", "text", "text or json")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		io.WriteString(stderr, credUsage)
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		io.WriteString(stderr, credUsage)
		return 2
	}
	cmd, name := rest[0], ""
	if len(rest) > 1 {
		name = rest[1]
	}
	switch cmd {
	case "set", "get", "list", "delete":
	default:
		fmt.Fprintf(stderr, "agentflow cred: unknown command %q\n\n", cmd)
		io.WriteString(stderr, credUsage)
		return 2
	}
	if (cmd == "set" || cmd == "get" || cmd == "delete") && name == "" {
		fmt.Fprintln(stderr, "agentflow cred: command", cmd, "requires a name")
		return 2
	}
	if cmd == "list" && name != "" {
		fmt.Fprintln(stderr, "agentflow cred: list takes no name")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintln(stderr, "agentflow cred: -format must be text or json")
		return 2
	}
	if *user != "" && cmd == "set" {
		// The identity store may not be enabled; a uuid is still a valid key.
		if !strings.HasPrefix(*user, "u_") {
			fmt.Fprintln(stderr, "agentflow cred: -user must be a profile uuid (u_...)")
			return 2
		}
	}

	lvl, _ := llog.ParseLevel("error") // CLI: quiet by default, errors only
	log := slog.New(llog.NewTextHandler(stderr, lvl))

	src, err := config.ResolveSource(*cfgPath, *configDir)
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
		return 0

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
		if *format == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"service": name, "user": *user, "value": sec.Value,
				"header": sec.Header, "scheme": sec.Scheme,
			})
		} else {
			fmt.Fprintln(stdout, sec.Value)
		}
		return 0

	case "list":
		refs, err := store.List(ctx, *user)
		if err != nil {
			fmt.Fprintln(stderr, "agentflow cred:", err)
			return 1
		}
		if *format == "json" {
			_ = json.NewEncoder(stdout).Encode(refs)
			return 0
		}
		for _, r := range refs {
			fmt.Fprintln(stdout, r.Service) // names only, never values
		}
		return 0

	case "delete":
		if err := store.Delete(ctx, *user, name); err != nil {
			fmt.Fprintln(stderr, "agentflow cred:", err)
			return 1
		}
		fmt.Fprintf(stdout, "deleted %s (user %q)\n", name, *user)
		return 0
	}

	io.WriteString(stderr, credUsage)
	return 2
}

// hoistFlags moves flag arguments (and their values) ahead of the positional
// arguments, so `cred set NAME -config X` parses like `cred -config X set
// NAME`. Go's flag package stops at the first non-flag argument; operators
// naturally write the command first. The CLI's flags all take values.
func hoistFlags(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags = append(flags, a, args[i+1])
			i++
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
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
