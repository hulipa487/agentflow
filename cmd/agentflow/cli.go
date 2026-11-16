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

// topCommand is one entry of `agentflow help`.
type topCommand struct {
	name, summary string
	run           func(args []string, stdout, stderr io.Writer) int
	// config reports whether the command takes -config, which is what lets
	// `agentflow -config X <command>` move the flag after the command.
	config bool
}

// topCommands is the command list, in the order help shows it. run, version
// and help are handled before the table is consulted.
var topCommands = []topCommand{
	{"run", "run the engine (also what `agentflow [flags]` does)", nil, true},
	{"init", "write a starter deployment", cmdInit, false},
	{"validate", "load a config and compile every loop", cmdValidate, true},
	{"doctor", "check config, loops, credentials, models and backends", cmdDoctor, true},
	{"config", "schema | path | show", cmdConfig, true},
	{"loop", "check | test: compile a loop or run a scenario", cmdLoop, false},
	{"tools", "import-openapi: turn an OpenAPI spec into HTTP tools", cmdTools, false},
	{"migrate", "store | blobs: copy state to another backend", cmdMigrate, true},
	{"user", "keygen | export | import: move one profile between cells", cmdUser, true},
	{"cred", "set | get | list | delete: manage stored credentials", cmdCred, true},
	{"support-bundle", "collect a redacted diagnostic archive", cmdSupport, true},
	{"version", "print version information", nil, false},
	{"help", "show a command's usage and flags", nil, false},
}

func lookupCommand(name string) (topCommand, bool) {
	for _, c := range topCommands {
		if c.name == name {
			return c, true
		}
	}
	return topCommand{}, false
}

// runSubcommand handles every command except the engine. It returns
// handled=false when the arguments run the engine: none at all, `run ...`, or
// flags first.
func runSubcommand(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "run":
		return false, 0
	case "-h", "-help", "--help":
		printUsage(stdout)
		return true, 0
	case "version", "-version", "--version":
		printVersion(stdout)
		return true, 0
	case "help":
		return true, cmdHelp(args[1:], stdout, stderr)
	}
	if c, ok := lookupCommand(args[0]); ok && c.run != nil {
		return true, c.run(args[1:], stdout, stderr)
	}
	// A flag-first invocation is the engine. A bare unknown word is a command
	// typo; booting the engine instead would be a surprising listening process.
	if strings.HasPrefix(args[0], "-") {
		return false, 0
	}
	fmt.Fprintf(stderr, "agentflow: unknown command %q\n\n", args[0])
	printUsage(stderr)
	return true, 2
}

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "agentflow %s\nprelude %d\ngo %s\n", version, vm.PreludeVersion, runtime.Version())
}

// hoistConfigFlags lets `agentflow -config deploy/ validate` work as well as
// `agentflow validate -config deploy/`: a leading -config moves after the
// command, for the commands that take one.
func hoistConfigFlags(args []string) []string {
	var flags []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-config" || a == "--config":
			if i+1 >= len(args) {
				return args
			}
			flags = append(flags, a, args[i+1])
			i++
		case strings.HasPrefix(a, "-config=") || strings.HasPrefix(a, "--config="):
			flags = append(flags, a)
		default:
			if c, ok := lookupCommand(a); len(flags) > 0 && ok && c.config {
				return append(append([]string{}, args[i:]...), flags...)
			}
			return args
		}
	}
	return args
}

// printUsage is `agentflow help` and `agentflow -h`.
func printUsage(w io.Writer) {
	fmt.Fprint(w, `agentflow: self-hosted LLM agent runtime

usage:
  agentflow [run] [flags]            run the engine
  agentflow <command> [flags] ...    run a command
  agentflow help <command>           show a command's usage and flags

commands:
`)
	for _, c := range topCommands {
		fmt.Fprintf(w, "  %-16s %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, `
examples:
  agentflow init mydeploy
  agentflow doctor -config mydeploy
  agentflow run -config mydeploy
  agentflow help run
`)
}

// cmdHelp prints a command's help by running it with -h, so the text comes
// from the flags the command really defines. `help config show` works the
// same way, one level down.
func cmdHelp(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}
	switch args[0] {
	case "run":
		_, code, _ := parseRunFlags([]string{"-h"}, stdout)
		return code
	case "version":
		fmt.Fprintln(stdout, "usage: agentflow version\n\nPrint the agentflow version, the prelude API version and the Go version.")
		return 0
	case "help":
		fmt.Fprintln(stdout, "usage: agentflow help [command [subcommand]]")
		return 0
	}
	c, ok := lookupCommand(args[0])
	if !ok || c.run == nil {
		fmt.Fprintf(stderr, "agentflow: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
	rest := append(append([]string{}, args[1:]...), "-h")
	if code := c.run(rest, stdout, stdout); code != 0 {
		return code
	}
	return 0
}

// loadConfigForCLI resolves and loads the same source the engine would use. It
// deliberately does not chdir: relative paths are rebased onto the config
// source during load, so validation and boot cannot disagree.
func loadConfigForCLI(cfgPath string, stderr io.Writer) (*config.Config, config.Source, error) {
	src, err := config.ResolveSource(cfgPath)
	if err != nil {
		return nil, config.Source{}, err
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
	info := cmdInfo{
		Name:    "validate",
		Summary: "Load the config, compile every agent and spawn-profile loop, and check the gateway route.",
		Detail:  "Relative paths resolve against the config file or directory.",
	}
	fs := newCommand(info, stderr)
	cfgPath := addConfigFlag(fs)
	if code, ok := parseArgs(fs, args, nil); !ok {
		return code
	}
	if !positionals(fs, info, 0) {
		return 2
	}
	cfg, used, err := loadConfigForCLI(*cfgPath, stderr)
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
	Status string `json:"status"` // PASS | WARN | FAIL | SKIP
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	info := cmdInfo{
		Name:    "doctor",
		Summary: "Check the config, loops, credentials, identity, admin authentication, models and channels.",
		Detail: `-online also connects to the runtime store, checks the pgvector extension,
and probes each S3 bucket, Redis and Qdrant backend. It is on by default when
runtime.topology is regional; -online=false turns it off.`,
	}
	fs := newCommand(info, stderr)
	cfgPath := addConfigFlag(fs)
	online := fs.Bool("online", false, "also check the runtime store, vector extension, S3, Redis and Qdrant (default on for regional)")
	asJSON := fs.Bool("json", false, "print the results as JSON")
	if code, ok := parseArgs(fs, args, nil); !ok {
		return code
	}
	if !positionals(fs, info, 0) {
		return 2
	}
	var results []doctorResult
	report := func() {
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(results)
			return
		}
		printDoctor(stdout, results)
	}
	add := func(status, name, detail string) {
		results = append(results, doctorResult{status, name, detail})
	}
	exit := 0
	cfg, used, err := loadConfigForCLI(*cfgPath, stderr)
	if err != nil {
		add("FAIL", "configuration", err.Error())
		report()
		return 1
	}
	builtins.SetPluginDir(cfg.Plugins.Dir)
	builtins.SetPackages(cfg.Packages)
	add("PASS", "configuration", used.String()+" (epoch "+cfg.Epoch+")")

	// A regional cell's failure modes are all "some instance cannot reach what
	// the config names", so the live checks default on there; single keeps the
	// old offline default and opts in with -online.
	onlineSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "online" {
			onlineSet = true
		}
	})
	if !onlineSet && cfg.IsRegional() {
		*online = true
	}

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

	// The admin API is on the shared listener. Without a configured
	// credential it runs on a per-boot token and answers local clients only.
	listen := cfg.Gateway.Listen
	if listen == "" {
		listen = ":8080"
	}
	hasToken := os.Getenv("ADMIN_TOKEN") != "" || len(cfg.Runtime.Admin.Operators) > 0
	switch {
	case hasToken:
		add("PASS", "admin", "a configured token guards the admin API on "+listen+" (/admin/)")
	case cfg.IsRegional():
		add("FAIL", "admin", "regional topology needs ADMIN_TOKEN or runtime.admin.operators: a per-boot token is unknown to the other instances")
		exit = 1
	case !cfg.Runtime.Admin.TCPEnabled():
		add("WARN", "admin", "ADMIN_TOKEN is unset; the socket-only admin API mints a per-boot token")
	case loopbackListen(listen):
		add("WARN", "admin", "ADMIN_TOKEN is unset; the admin API mints a per-boot token")
	default:
		add("WARN", "admin", "ADMIN_TOKEN is unset: the admin API on "+listen+" answers local clients only, on a per-boot token")
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
	for i, ch := range cfg.Gateway.Channels {
		if ch.Type != "openai" {
			continue
		}
		name := ch.Name
		if name == "" {
			name = fmt.Sprintf("openai-%d", i)
		}
		agents := ch.Agents
		if len(agents) == 0 && ch.Agent != "" {
			agents = []string{ch.Agent}
		}
		switch ch.Auth {
		case "none":
			add("WARN", "channel "+name, "openai with auth: none — anyone who can reach the listener can spend the models (loopback only is enforced at validation)")
		case "user_token":
			add("PASS", "channel "+name, fmt.Sprintf("openai, auth user_token, %d agents: %s", len(agents), strings.Join(agents, ", ")))
		default:
			add("PASS", "channel "+name, fmt.Sprintf("openai, auth key, %d agents: %s", len(agents), strings.Join(agents, ", ")))
		}
	}
	if *online {
		if runDoctorOnline(cfg, add) {
			exit = 1
		}
	} else {
		add("SKIP", "network", "no provider connectivity was attempted; pass -online to check backends")
	}
	report()
	return exit
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
	return runGroup("config", []subcommand{
		{"schema", "print the JSON schema for agentflow.yaml", cmdConfigSchema},
		{"path", "print the resolved config file or directory", cmdConfigPath},
		{"show", "summarize the resolved config (paths, epoch, counts)", cmdConfigShow},
	}, args, stdout, stderr)
}

func cmdConfigSchema(args []string, stdout, stderr io.Writer) int {
	info := cmdInfo{Name: "config schema", Summary: "Print the JSON schema for agentflow.yaml, for editor validation."}
	fs := newCommand(info, stderr)
	out := fs.String("out", "", "write to this file instead of stdout")
	if code, ok := parseArgs(fs, args, nil); !ok {
		return code
	}
	if !positionals(fs, info, 0) {
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
	if err := writeOutput(*out, b, 0o644, stdout); err != nil {
		fmt.Fprintln(stderr, "schema:", err)
		return 1
	}
	if *out != "" {
		fmt.Fprintln(stdout, "wrote", *out)
	}
	return 0
}

func cmdConfigPath(args []string, stdout, stderr io.Writer) int {
	info := cmdInfo{Name: "config path", Summary: "Print the config file or directory the engine would load."}
	fs := newCommand(info, stderr)
	cfgPath := addConfigFlag(fs)
	if code, ok := parseArgs(fs, args, nil); !ok {
		return code
	}
	if !positionals(fs, info, 0) {
		return 2
	}
	src, err := config.ResolveSource(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "config path:", err)
		return 1
	}
	if src.Legacy {
		fmt.Fprintln(stderr, "warning: using config.yaml; rename it to agentflow.yaml")
	}
	fmt.Fprintln(stdout, src.String())
	return 0
}

// configSummary is what `config show` reports. It holds no value from a
// secret-bearing field.
type configSummary struct {
	Source      string         `json:"source"`
	Kind        string         `json:"kind"`
	Epoch       string         `json:"epoch"`
	Persistence string         `json:"persistence"`
	Agents      int            `json:"agents"`
	Channels    int            `json:"channels"`
	Models      []summaryModel `json:"models"`
	Loops       []summaryLoop  `json:"loops"`
	Listen      string         `json:"listen"`    // gateway.listen: the one HTTP port
	AdminTCP    bool           `json:"admin_tcp"` // admin API served on it, under /admin/
	AdminSocket string         `json:"admin_socket,omitempty"`
}

type summaryModel struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type summaryLoop struct {
	Name string `json:"name"` // agent name, spawn:<profile> or gateway.route
	Path string `json:"path"`
}

// cmdConfigShow answers the question operators actually have: which config did
// this process resolve, and where did its relative loop paths land?
func cmdConfigShow(args []string, stdout, stderr io.Writer) int {
	info := cmdInfo{Name: "config show", Summary: "Summarize the resolved config: source, epoch, counts, models, loop paths and admin address."}
	fs := newCommand(info, stderr)
	cfgPath := addConfigFlag(fs)
	asJSON := fs.Bool("json", false, "print the summary as JSON")
	if code, ok := parseArgs(fs, args, nil); !ok {
		return code
	}
	if !positionals(fs, info, 0) {
		return 2
	}
	cfg, src, err := loadConfigForCLI(*cfgPath, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "config show:", err)
		return 1
	}
	sum := configSummary{
		Source:      src.String(),
		Kind:        "file",
		Epoch:       cfg.Epoch,
		Persistence: cfg.Runtime.Persistence,
		Agents:      len(cfg.Agents),
		Channels:    len(cfg.Gateway.Channels),
		Models:      []summaryModel{},
		Loops:       []summaryLoop{},
		Listen:      cfg.Gateway.Listen,
		AdminTCP:    cfg.Runtime.Admin.TCPEnabled(),
		AdminSocket: cfg.Runtime.Admin.Socket,
	}
	if src.IsDir {
		sum.Kind = "directory"
	}
	// The scheme only: a DSN carries a password.
	if i := strings.IndexByte(sum.Persistence, ':'); i >= 0 {
		sum.Persistence = sum.Persistence[:i]
	}
	if sum.Persistence == "" {
		sum.Persistence = "default"
	}
	if sum.Listen == "" {
		sum.Listen = ":8080"
	}
	for _, name := range sortedKeys(cfg.Models) {
		sum.Models = append(sum.Models, summaryModel{name, cfg.Models[name].Provider, cfg.Models[name].Model})
	}
	for _, name := range sortedAgentNames(cfg) {
		sum.Loops = append(sum.Loops, summaryLoop{name, cfg.Agents[name].Loop})
	}
	for _, name := range sortedKeys(cfg.Profiles.Agent) {
		sum.Loops = append(sum.Loops, summaryLoop{"spawn:" + name, cfg.Profiles.Agent[name].Loop})
	}
	if cfg.Gateway.Route != "" {
		sum.Loops = append(sum.Loops, summaryLoop{"gateway.route", cfg.Gateway.Route})
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(sum)
		return 0
	}
	fmt.Fprintf(stdout, "source: %s\nkind: %s\nepoch: %s\n", sum.Source, sum.Kind, sum.Epoch)
	fmt.Fprintf(stdout, "persistence: %s\n", sum.Persistence)
	fmt.Fprintf(stdout, "agents: %d\nchannels: %d\n", sum.Agents, sum.Channels)
	for _, m := range sum.Models {
		fmt.Fprintf(stdout, "model: %s (%s/%s)\n", m.Name, m.Provider, m.Model)
	}
	for _, l := range sum.Loops {
		fmt.Fprintf(stdout, "loop.%s: %s\n", l.Name, l.Path)
	}
	socket := sum.AdminSocket
	if socket == "" {
		socket = "(disabled)"
	}
	fmt.Fprintf(stdout, "listen: %s\nadmin: /admin/ (on the listener: %v)\nadmin.socket: %s\n", sum.Listen, sum.AdminTCP, socket)
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
	return runGroup("loop", []subcommand{
		{"check", "compile a loop file, a directory of loops, or a builtin plugin", cmdLoopCheck},
		{"test", "run a loop test scenario", cmdLoopTest},
	}, args, stdout, stderr)
}

func cmdLoopCheck(args []string, stdout, stderr io.Writer) int {
	info := cmdInfo{
		Name:    "loop check",
		Args:    "FILE|DIR|plugin:NAME",
		Summary: "Compile a loop and check the prelude version it declares.",
	}
	fs := newCommand(info, stderr)
	if code, ok := parseArgs(fs, args, nil); !ok {
		return code
	}
	if !positionals(fs, info, 1) {
		return 2
	}
	ref := fs.Arg(0)
	if err := checkLoop(ref, ref); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "ok:", ref)
	return 0
}

func cmdLoopTest(args []string, stdout, stderr io.Writer) int {
	info := cmdInfo{
		Name:    "loop test",
		Args:    "SCENARIO.yaml",
		Summary: "Run a loop against the scripted messages and expectations in a scenario file.",
	}
	fs := newCommand(info, stderr)
	if code, ok := parseArgs(fs, args, nil); !ok {
		return code
	}
	if !positionals(fs, info, 1) {
		return 2
	}
	path := fs.Arg(0)
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "loop test:", err)
		return 1
	}
	sc, err := looptest.LoadFile(path, b)
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
	info := cmdInfo{
		Name:    "init",
		Args:    "[DIR]",
		Summary: "Write a runnable starter deployment into DIR (default: the current directory).",
		Detail: `The default layout is one agentflow.yaml; -layout dir writes system.yaml,
channels.yaml and profiles/*.yaml instead. Either way, -config DIR loads it.`,
	}
	fs := newCommand(info, stderr)
	layout := fs.String("layout", "file", "file (one agentflow.yaml) or dir (system.yaml, channels.yaml, profiles/)")
	force := fs.Bool("force", false, "overwrite existing files")
	if code, ok := parseArgs(fs, args, map[string]string{
		"dir":       "pass the directory as an argument: agentflow init DIR",
		"configdir": "use -layout dir",
	}); !ok {
		return code
	}
	if fs.NArg() > 1 {
		positionals(fs, info, 1)
		return 2
	}
	target := "."
	if fs.NArg() == 1 {
		target = fs.Arg(0)
	}
	dir := &target
	if *layout != "file" && *layout != "dir" {
		fmt.Fprintln(stderr, "agentflow init: -layout must be file or dir")
		return 2
	}
	useDir := *layout == "dir"
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	// The directive comes from vm.PreludeVersion so a version bump cannot leave
	// init writing a loop that this same binary refuses to load.
	loop := fmt.Sprintf("-- af-prelude-version: %d\n", vm.PreludeVersion) + `-- Echo loop: the smallest runnable agent. Replace it with your own behavior.
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

  # OpenAI-compatible endpoint: point any Chat Completions client at
  # http://<host>:8080/v1 with the key below. Uncomment and set the key.
  # - name: api
  #   type: openai
  #   path: /v1/
  #   agents: [echo]
  #   auth: key
  #   keys: ["${COMPAT_API_KEY}"]
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

    # OpenAI-compatible endpoint: point any Chat Completions client at
    # http://<host>:8080/v1 with the key below. Uncomment and set the key.
    # - name: api
    #   type: openai
    #   path: /v1/
    #   agents: [echo]
    #   auth: key
    #   keys: ["${COMPAT_API_KEY}"]

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
	} else {
		fmt.Fprintln(stdout, "initialized", filepath.Join(*dir, "agentflow.yaml"))
	}
	if filepath.Clean(*dir) == "." {
		fmt.Fprintln(stdout, "next: agentflow doctor")
		fmt.Fprintln(stdout, "then: agentflow run")
	} else {
		fmt.Fprintln(stdout, "next: agentflow doctor -config", *dir)
		fmt.Fprintln(stdout, "then: agentflow run -config", *dir)
	}
	return 0
}

// runFlags are the engine's flags: `agentflow run [flags]`, or the same flags
// with no command.
type runFlags struct {
	config      string
	logLevel    string
	logFormat   string
	logFile     string
	noTUI       bool
	noConsole   bool
	forceSingle bool
}

func parseRunFlags(args []string, w io.Writer) (runFlags, int, bool) {
	info := cmdInfo{
		Name:    "run",
		Summary: "Run the engine. `agentflow [flags]` with no command does the same.",
		Detail: `The worker pool size is runtime.scheduler.workers in the config (default 8).
Set ADMIN_TOKEN to fix the admin API token; without it a loopback-only
console mints one per boot.`,
	}
	fs := newCommand(info, w)
	var f runFlags
	fs.StringVar(&f.config, "config", "", "config file or directory (default: agentflow.yaml, then config.yaml)")
	fs.StringVar(&f.logLevel, "log-level", "info", "minimum log level: dev, debug, info, warn or error")
	fs.StringVar(&f.logFormat, "log-format", "text", "log format: text or json")
	fs.StringVar(&f.logFile, "log-file", "", "also append logs to this file")
	fs.BoolVar(&f.noTUI, "no-tui", false, "no terminal dashboard; plain logs on stderr")
	fs.BoolVar(&f.noConsole, "no-console", false, "serve the admin API without the static web console")
	fs.BoolVar(&f.forceSingle, "force-single", false, "start even if another instance looks live on the store (single topology; only after a crash)")
	code, ok := parseArgs(fs, args, map[string]string{"version": "run `agentflow version`"})
	if !ok {
		return f, code, false
	}
	if !positionals(fs, info, 0) {
		return f, 2, false
	}
	return f, 0, true
}
