package main

// Subcommands are deliberately separate from the long-running engine. Each one
// either reads configuration (validate, doctor, schema) or writes a starter
// deployment (init); none of them opens a listener or a store. The engine path
// remains the no-subcommand path, so existing invocations are unchanged.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/invopop/jsonschema"

	"agentflow/internal/builtins"
	"agentflow/internal/config"
	"agentflow/internal/looptest"
	"agentflow/internal/vm"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runSubcommand handles the standalone commands. It returns handled=false for
// the engine path, which keeps `agentflow -config ...` behaving exactly as it
// did before.
func runSubcommand(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "help":
		return true, cmdHelp(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintf(stdout, "agentflow %s\nprelude %d\ngo %s\n", version, vm.PreludeVersion, runtime.Version())
		return true, 0
	case "cred":
		return true, credMain(args[1:], os.Stdin, stdout, stderr)
	case "validate":
		return true, cmdValidate(args[1:], stdout, stderr)
	case "doctor":
		return true, cmdDoctor(args[1:], stdout, stderr)
	case "init":
		return true, cmdInit(args[1:], stdout, stderr)
	case "config":
		return true, cmdConfig(args[1:], stdout, stderr)
	case "loop":
		return true, cmdLoop(args[1:], stdout, stderr)
	case "tools":
		return true, cmdTools(args[1:], stdout, stderr)
	case "support-bundle":
		return true, cmdSupport(args[1:], stdout, stderr)
	default:
		// A flag-first invocation is the engine. A bare unknown word is a
		// command typo; booting the engine instead would be a surprising
		// listening process.
		if !strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(stderr, "agentflow: unknown command %q\n\n", args[0])
			printUsage(stderr)
			return true, 2
		}
		return false, 0
	}
}

// hoistConfigFlags lets `agentflow -config deploy/agentflow.yaml validate`
// work as well as `agentflow validate -config ...`. Only the two config flags
// are hoisted; engine-only flags stay on the engine path, and init is excluded
// because its -configdir is a different (deprecated boolean) flag.
func hoistConfigFlags(args []string) []string {
	if len(args) == 0 || args[0] != "-config" && args[0] != "--config" &&
		args[0] != "-configdir" && args[0] != "--configdir" &&
		!strings.HasPrefix(args[0], "-config=") && !strings.HasPrefix(args[0], "--config=") &&
		!strings.HasPrefix(args[0], "-configdir=") && !strings.HasPrefix(args[0], "--configdir=") {
		return args
	}
	var flags []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-config" || a == "--config" || a == "-configdir" || a == "--configdir":
			if i+1 >= len(args) {
				return args
			}
			flags = append(flags, a, args[i+1])
			i += 2
		case strings.HasPrefix(a, "-config=") || strings.HasPrefix(a, "--config=") ||
			strings.HasPrefix(a, "-configdir=") || strings.HasPrefix(a, "--configdir="):
			flags = append(flags, a)
			i++
		default:
			if i < len(args) && isCommand(a) && a != "init" && a != "help" {
				return append(append([]string{}, args[i:]...), flags...)
			}
			return args
		}
	}
	return args
}

func isCommand(name string) bool {
	switch name {
	case "help", "version", "cred", "validate", "doctor", "init", "config", "loop", "tools", "support-bundle":
		return true
	}
	return false
}

// printUsage is both `agentflow -h` and `agentflow help`. `-config` is
// deliberately shown first: it is the one flag every mode shares.
func printUsage(w io.Writer) {
	fmt.Fprint(w, `agentflow — self-hosted LLM agent runtime

usage:
  agentflow [flags]                 run the engine
  agentflow <command> [flags]       run a standalone command
  agentflow help [command]          show this help or one command's usage

commands:
  init          write a starter deployment (single file or config directory)
  validate      load a config and compile every loop
  doctor        check config, loops, credentials and model references
  config        schema | path | show
  loop          check <file|dir|plugin:name> | test <scenario.yaml>
  tools         import-openapi <spec> [-out dir]
  cred          set|get|list|delete <name>
  support-bundle  collect diagnostics for a support request
  version       print version information
  help          show help for a command

engine flags:
  -config PATH        config file or directory (default: agentflow.yaml, then config.yaml)
  -configdir DIR      deprecated alias for -config DIR
  -workers N          op pool size; overrides runtime.scheduler.workers
  -log-level LEVEL    dev|debug|info|warn|error (default info)
  -log-format FORMAT  text|json (default text)
  -log-file PATH      append logs to a file as well as stderr
  -log-json           deprecated alias for -log-format json
  -no-tui             disable the terminal dashboard
  -no-console         serve the admin and Studio APIs without the static web console
  -no-webui           deprecated alias for -no-console
  -version            print version and exit

examples:
  agentflow init
  agentflow validate -config deploy/agentflow.yaml
  agentflow doctor -config deploy/
  agentflow -config deploy/agentflow.yaml validate
  agentflow help config
`)
}

var commandHelp = map[string]string{
	"init": `usage: agentflow init [-dir DIR] [-layout file|dir] [-force]

Write a runnable starter deployment. The default layout is one agentflow.yaml;
-layout dir writes system.yaml, channels.yaml and profiles/*.yaml instead.
-configdir is accepted as a deprecated boolean alias for -layout dir.
`,
	"validate": `usage: agentflow validate [-config PATH | -configdir DIR]

Load the config, compile every agent and spawn-profile loop, and check the
gateway route. Relative paths resolve against the config file or directory.
`,
	"doctor": `usage: agentflow doctor [-config PATH | -configdir DIR]

Offline checks: config load, loop compilation, credentials, identity, admin
authentication, models and channels. It does not call a provider.
`,
	"config": `usage: agentflow config <command> [-config PATH | -configdir DIR]

commands:
  agentflow config schema [-out FILE]   print the JSON schema for agentflow.yaml
  agentflow config path                 print the resolved config file or directory
  agentflow config show                 summarize the resolved config (paths, epoch, counts)
`,
	"loop": `usage: agentflow loop check <file|dir|plugin:name>
       agentflow loop test <scenario.yaml>
`,
	"tools": `usage: agentflow tools import-openapi <spec> [-out DIR]

Convert an OpenAPI document into declarative tools.http entries.
`,
	"support-bundle": `usage: agentflow support-bundle [-config PATH | -configdir DIR] [-out FILE]

Collect a redacted diagnostic archive for a support request.
`,
	"cred": `usage: agentflow cred <command> [flags] <name>

commands: set, get, list, delete. Values are read from a prompt or stdin,
never argv.
`,
	"version": "usage: agentflow version\n",
	"help":    "usage: agentflow help [command]\n",
}

func cmdHelp(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}
	text, ok := commandHelp[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "agentflow: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
	fmt.Fprint(stdout, text)
	return 0
}

// hoistFlagsWithBools moves flags ahead of positionals, like cred's hoistFlags,
// but knows which flags are boolean so it does not swallow the next argument.
func hoistFlagsWithBools(args []string, bools map[string]bool) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if bools[a] {
				flags = append(flags, a)
				continue
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				flags = append(flags, a, args[i+1])
				i++
				continue
			}
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

func cliConfigFlags(name string, stderr io.Writer) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := fs.String("config", "", "config file or directory (default: agentflow.yaml, then config.yaml)")
	dir := fs.String("configdir", "", "deprecated: config directory (use -config DIR)")
	return fs, cfg, dir
}

// loadConfigForCLI resolves and loads the same source the engine would use. It
// deliberately does not chdir: relative paths are rebased onto the config
// source during load, so validation and boot cannot disagree.
func loadConfigForCLI(cfgPath, configDir string, stderr io.Writer) (*config.Config, config.Source, error) {
	src, err := config.ResolveSource(cfgPath, configDir)
	if err != nil {
		return nil, config.Source{}, err
	}
	if configDir != "" {
		fmt.Fprintln(stderr, "warning: -configdir is deprecated; use -config DIR")
	}
	if src.Legacy {
		fmt.Fprintln(stderr, "warning: using config.yaml; rename it to agentflow.yaml")
	}
	c, err := src.Load(discardLogger())
	return c, src, err
}

// checkLoop resolves a loop/route reference and compiles it. Resolve already
// enforces the prelude version, which is the failure a first-time author hits
// most often.
func checkLoop(label, ref string) error {
	src, _, err := builtins.Resolve(ref)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := vm.CompileCheck(ref, src); err != nil {
		return fmt.Errorf("%s: compile: %w", label, err)
	}
	return nil
}

func validateLoops(cfg *config.Config) error {
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := checkLoop("agent "+name, cfg.Agents[name].Loop); err != nil {
			return err
		}
	}
	pnames := make([]string, 0, len(cfg.Profiles.Agent))
	for name := range cfg.Profiles.Agent {
		pnames = append(pnames, name)
	}
	sort.Strings(pnames)
	for _, name := range pnames {
		if err := checkLoop("spawn profile "+name, cfg.Profiles.Agent[name].Loop); err != nil {
			return err
		}
	}
	if cfg.Gateway.Route != "" {
		if err := checkLoop("gateway.route", cfg.Gateway.Route); err != nil {
			return err
		}
	}
	return nil
}

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath, configDir := cliConfigFlags("validate", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "validate: unexpected argument:", fs.Arg(0))
		return 2
	}
	cfg, used, err := loadConfigForCLI(*cfgPath, *configDir, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return 1
	}
	builtins.SetPluginDir(cfg.Plugins.Dir)
	builtins.SetPackages(cfg.Packages)
	if err := validateLoops(cfg); err != nil {
		fmt.Fprintln(stderr, "loop:", err)
		return 1
	}
	fmt.Fprintf(stdout, "ok: %s (epoch %s, %d agents, %d spawn profiles)\n", used.String(), cfg.Epoch, len(cfg.Agents), len(cfg.Profiles.Agent))
	return 0
}

type doctorResult struct {
	Status string // PASS | WARN | FAIL | SKIP
	Name   string
	Detail string
}

func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath, configDir := cliConfigFlags("doctor", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "doctor: unexpected argument:", fs.Arg(0))
		return 2
	}
	var results []doctorResult
	add := func(status, name, detail string) {
		results = append(results, doctorResult{status, name, detail})
	}
	exit := 0
	cfg, used, err := loadConfigForCLI(*cfgPath, *configDir, stderr)
	if err != nil {
		add("FAIL", "configuration", err.Error())
		printDoctor(stdout, results)
		return 1
	}
	builtins.SetPluginDir(cfg.Plugins.Dir)
	builtins.SetPackages(cfg.Packages)
	add("PASS", "configuration", used.String()+" (epoch "+cfg.Epoch+")")

	if err := validateLoops(cfg); err != nil {
		add("FAIL", "loops", err.Error())
		exit = 1
	} else {
		add("PASS", "loops", fmt.Sprintf("%d agent loops + %d spawn-profile loops compile", len(cfg.Agents), len(cfg.Profiles.Agent)))
	}

	if cfg.Runtime.Credentials.Enabled {
		env := cfg.CredentialsMasterKeyEnv()
		if os.Getenv(env) == "" {
			add("FAIL", "credentials", "runtime.credentials.enabled but "+env+" is unset")
			exit = 1
		} else {
			add("PASS", "credentials", "master key present in "+env)
		}
	} else {
		add("SKIP", "credentials", "credential store disabled")
	}

	if cfg.Runtime.Identity.Enabled {
		add("PASS", "identity", "enabled ("+cfg.IdentityStore()+")")
	} else {
		add("SKIP", "identity", "identity layer disabled (anonymous/service stratum)")
	}

	adminListen := cfg.Runtime.Admin.Listen
	if adminListen == "" {
		adminListen = "127.0.0.1:9090"
	}
	if !loopbackListen(adminListen) && os.Getenv("ADMIN_TOKEN") == "" {
		add("FAIL", "admin", "runtime.admin.listen is not loopback and ADMIN_TOKEN is unset")
		exit = 1
	} else if os.Getenv("ADMIN_TOKEN") == "" {
		add("WARN", "admin", "ADMIN_TOKEN is unset; the console mints a per-boot token")
	} else {
		add("PASS", "admin", "ADMIN_TOKEN is set")
	}

	for _, name := range sortedKeys(cfg.Models) {
		m := cfg.Models[name]
		switch {
		case m.UserKeyed():
			add("PASS", "model "+name, "uses the acting user's key ("+m.CredentialService()+")")
		case strings.TrimSpace(m.APIKey) == "":
			add("WARN", "model "+name, "no api_key; the first call will fail")
		default:
			add("PASS", "model "+name, m.Provider+"/"+m.Model)
		}
	}
	for _, name := range sortedAgentNames(cfg) {
		a := cfg.Agents[name]
		if a.Model == "" {
			continue
		}
		if _, ok := cfg.Models[a.Model]; !ok {
			add("FAIL", "agent "+name, "references unknown model "+a.Model)
			exit = 1
		}
	}
	if len(cfg.Gateway.Channels) == 0 {
		add("WARN", "channels", "no channels configured; only API/Studio traffic can reach the agents")
	} else {
		add("PASS", "channels", fmt.Sprintf("%d configured", len(cfg.Gateway.Channels)))
	}
	add("SKIP", "network", "no provider connectivity was attempted (doctor is offline by design)")
	printDoctor(stdout, results)
	if exit != 0 {
		return exit
	}
	return 0
}

func printDoctor(w io.Writer, results []doctorResult) {
	for _, r := range results {
		fmt.Fprintf(w, "%-4s %-16s %s\n", r.Status, r.Name, r.Detail)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedAgentNames(cfg *config.Config) []string { return sortedKeys(cfg.Agents) }

func cmdConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: agentflow config <schema|path|show> [flags]")
		return 2
	}
	switch args[0] {
	case "path":
		return cmdConfigPath(args[1:], stdout, stderr)
	case "show":
		return cmdConfigShow(args[1:], stdout, stderr)
	case "schema":
	default:
		fmt.Fprintln(stderr, "usage: agentflow config <schema|path|show> [flags]")
		return 2
	}
	fs := flag.NewFlagSet("config schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "write the schema to this file instead of stdout")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "config schema: unexpected argument:", fs.Arg(0))
		return 2
	}
	reflector := &jsonschema.Reflector{
		FieldNameTag:              "yaml",
		AllowAdditionalProperties: true,
	}
	schema := reflector.Reflect(&config.Config{})
	schema.Version = jsonschema.Version
	// Config structs have no omitempty convention on their YAML tags, so the
	// reflector would mark every field required. That is wrong for a config
	// where nearly everything is optional; remove the required arrays.
	dropRequired(schema)
	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "schema:", err)
		return 1
	}
	b = append(b, '\n')
	if *out == "" {
		_, _ = stdout.Write(b)
		return 0
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(stderr, "schema:", err)
		return 1
	}
	fmt.Fprintln(stdout, "wrote", *out)
	return 0
}

func cmdConfigPath(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath, configDir := cliConfigFlags("config path", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "config path: unexpected argument:", fs.Arg(0))
		return 2
	}
	src, err := config.ResolveSource(*cfgPath, *configDir)
	if err != nil {
		fmt.Fprintln(stderr, "config path:", err)
		return 1
	}
	if *configDir != "" {
		fmt.Fprintln(stderr, "warning: -configdir is deprecated; use -config DIR")
	}
	if src.Legacy {
		fmt.Fprintln(stderr, "warning: using config.yaml; rename it to agentflow.yaml")
	}
	fmt.Fprintln(stdout, src.String())
	return 0
}

// cmdConfigShow answers the question operators actually have: which config did
// this process resolve, and where did its relative loop paths land? It prints
// no values from secret-bearing fields.
func cmdConfigShow(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath, configDir := cliConfigFlags("config show", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "config show: unexpected argument:", fs.Arg(0))
		return 2
	}
	cfg, src, err := loadConfigForCLI(*cfgPath, *configDir, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "config show:", err)
		return 1
	}
	kind := "file"
	if src.IsDir {
		kind = "directory"
	}
	fmt.Fprintf(stdout, "source: %s\nkind: %s\nepoch: %s\n", src.String(), kind, cfg.Epoch)
	scheme := cfg.Runtime.Persistence
	if i := strings.IndexByte(scheme, ':'); i >= 0 {
		scheme = scheme[:i]
	}
	if scheme == "" {
		scheme = "default"
	}
	fmt.Fprintf(stdout, "persistence: %s\n", scheme)
	fmt.Fprintf(stdout, "agents: %d\nchannels: %d\n", len(cfg.Agents), len(cfg.Gateway.Channels))
	for _, name := range sortedKeys(cfg.Models) {
		fmt.Fprintf(stdout, "model: %s (%s/%s)\n", name, cfg.Models[name].Provider, cfg.Models[name].Model)
	}
	for _, name := range sortedAgentNames(cfg) {
		fmt.Fprintf(stdout, "loop.%s: %s\n", name, cfg.Agents[name].Loop)
	}
	for _, name := range sortedKeys(cfg.Profiles.Agent) {
		fmt.Fprintf(stdout, "loop.spawn:%s: %s\n", name, cfg.Profiles.Agent[name].Loop)
	}
	if cfg.Gateway.Route != "" {
		fmt.Fprintf(stdout, "loop.gateway.route: %s\n", cfg.Gateway.Route)
	}
	listen := cfg.Runtime.Admin.Listen
	if listen == "" {
		listen = "127.0.0.1:9090"
	}
	socket := cfg.Runtime.Admin.Socket
	if socket == "" {
		socket = "(disabled)"
	}
	fmt.Fprintf(stdout, "admin.listen: %s (tcp: %v)\nadmin.socket: %s\n", listen, cfg.Runtime.Admin.TCPEnabled(), socket)
	return 0
}

// dropRequired removes every `required` array from a reflected schema. Config
// zero values are meaningful and nearly every key is optional; the reflector
// cannot know that from the YAML tags, so marking everything required would
// make editor validation actively wrong.
func dropRequired(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	s.Required = nil
	if s.Properties != nil {
		for _, v := range s.Properties.FromOldest() {
			dropRequired(v)
		}
	}
	for _, v := range s.Definitions {
		dropRequired(v)
	}
	for _, v := range s.AllOf {
		dropRequired(v)
	}
	for _, v := range s.AnyOf {
		dropRequired(v)
	}
	for _, v := range s.OneOf {
		dropRequired(v)
	}
	dropRequired(s.Not)
	dropRequired(s.If)
	dropRequired(s.Then)
	dropRequired(s.Else)
	dropRequired(s.Items)
	dropRequired(s.Contains)
	dropRequired(s.AdditionalProperties)
	dropRequired(s.PropertyNames)
	for _, v := range s.PatternProperties {
		dropRequired(v)
	}
	for _, v := range s.DependentSchemas {
		dropRequired(v)
	}
}

func cmdLoop(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "check" && args[0] != "test") {
		fmt.Fprintln(stderr, "usage: agentflow loop check <file|dir|plugin:name>")
		fmt.Fprintln(stderr, "       agentflow loop test <scenario.yaml>")
		return 2
	}
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: agentflow loop check <file|dir|plugin:name>")
		fmt.Fprintln(stderr, "       agentflow loop test <scenario.yaml>")
		return 2
	}
	if args[0] == "check" {
		if err := checkLoop(args[1], args[1]); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "ok:", args[1])
		return 0
	}
	b, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(stderr, "loop test:", err)
		return 1
	}
	sc, err := looptest.LoadFile(args[1], b)
	if err != nil {
		fmt.Fprintln(stderr, "loop test:", err)
		return 1
	}
	if err := looptest.Run(sc, stdout); err != nil {
		fmt.Fprintln(stderr, "loop test:", err)
		return 1
	}
	return 0
}

func cmdInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "directory to initialize")
	layout := fs.String("layout", "file", "file (one agentflow.yaml) or dir (system.yaml + profiles/)")
	legacyDir := fs.Bool("configdir", false, "deprecated: same as -layout dir")
	force := fs.Bool("force", false, "overwrite existing files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "init: unexpected argument:", fs.Arg(0))
		return 2
	}
	useDir := *layout == "dir" || *legacyDir
	if *layout != "file" && *layout != "dir" {
		fmt.Fprintln(stderr, "init: -layout must be file or dir")
		return 2
	}
	if *legacyDir {
		fmt.Fprintln(stderr, "warning: init -configdir is deprecated; use -layout dir")
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	loop := `-- af-prelude-version: 1
-- Echo loop: the smallest runnable agent. Replace it with your own behavior.
function loop()
  while true do
    local msg = session.inbox()
    session.send("echo: " .. (msg.text or ""))
  end
end
`
	files := map[string]string{
		filepath.Join(*dir, "loops", "echo.lua"): loop,
		filepath.Join(*dir, ".env.example"): `# Copy to .env (or export) before running.
ADMIN_TOKEN=change-me
# Required only when runtime.credentials.enabled is true.
CREDENTIALS_MASTER_KEY=change-me
`,
	}
	if useDir {
		files[filepath.Join(*dir, "system.yaml")] = `version: "1"

runtime:
  persistence: sqlite://./data/agentflow.db

gateway:
  listen: ":8080"
`
		files[filepath.Join(*dir, "channels.yaml")] = `channels:
  - name: local
    type: webhook
    agent: echo
    path: /webhook/local/
`
		files[filepath.Join(*dir, "profiles", "echo.yaml")] = `name: echo
loop: ./loops/echo.lua
`
	} else {
		files[filepath.Join(*dir, "agentflow.yaml")] = `version: "1"

runtime:
  persistence: sqlite://./data/agentflow.db

gateway:
  listen: ":8080"
  channels:
    - name: local
      type: webhook
      agent: echo
      path: /webhook/local/

agents:
  echo:
    loop: ./loops/echo.lua
`
	}
	keys := sortedKeys(files)
	for _, path := range keys {
		if !*force {
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(stderr, "init: %s exists (use -force to overwrite)\n", path)
				return 1
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintln(stderr, "init:", err)
			return 1
		}
		if err := os.WriteFile(path, []byte(files[path]), 0o644); err != nil {
			fmt.Fprintln(stderr, "init:", err)
			return 1
		}
	}
	if useDir {
		fmt.Fprintln(stdout, "initialized config directory in", *dir)
		fmt.Fprintln(stdout, "next: agentflow doctor -config", *dir)
		fmt.Fprintln(stdout, "then:  agentflow -config", *dir)
	} else {
		fmt.Fprintln(stdout, "initialized", filepath.Join(*dir, "agentflow.yaml"))
		fmt.Fprintln(stdout, "next: agentflow doctor")
		fmt.Fprintln(stdout, "then:  agentflow")
	}
	return 0
}
