// agentflow — phase 2 entrypoint: memory, tools, MCP, router, channels, llm.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/mattn/go-isatty"

	"agentflow/internal/builtins"

	"agentflow/internal/config"
	"agentflow/internal/core/budget"
	"agentflow/internal/core/caps"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/files"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/media"
	"agentflow/internal/core/memory"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/reload"
	"agentflow/internal/core/router"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/safety"
	"agentflow/internal/core/scheduler"
	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/core/tools"
	"agentflow/internal/core/triggers"
	"agentflow/internal/drivers/browser"
	"agentflow/internal/drivers/fetch"
	"agentflow/internal/drivers/ghhook"
	"agentflow/internal/drivers/httpd"
	"agentflow/internal/drivers/legal"
	"agentflow/internal/drivers/llm"
	"agentflow/internal/drivers/mcp"
	"agentflow/internal/drivers/mongodb"
	"agentflow/internal/drivers/pgvector"
	"agentflow/internal/drivers/postgres"
	"agentflow/internal/drivers/qdrant"
	"agentflow/internal/drivers/redis"
	"agentflow/internal/drivers/redisvector"
	"agentflow/internal/drivers/s3media"
	"agentflow/internal/drivers/search"
	"agentflow/internal/drivers/shell"
	"agentflow/internal/drivers/sqlite"
	"agentflow/internal/drivers/telegram"
	"agentflow/internal/drivers/volatile"
	"agentflow/internal/drivers/webhook"
	"agentflow/internal/llog"
	"agentflow/internal/tui"
	"agentflow/internal/webui"
)

// version is stamped via -ldflags "-X main.version=..." on release builds.
var version = "dev"

func main() {
	cfgPath := flag.String("config", "agentflow.yaml", "path to agentflow.yaml")
	configDir := flag.String("configdir", "", "path to a config directory (alternative to -config; mutually exclusive)")
	workers := flag.Int("workers", 8, "op worker pool size")
	logLevel := flag.String("log-level", "info", "minimum log level: dev|debug|info|warn|error (additive)")
	noTUI := flag.Bool("no-tui", false, "disable the terminal dashboard (plain stderr logs)")
	noWebUI := flag.Bool("no-webui", false, "disable the web console (admin server keeps token-optional loopback behavior)")
	flag.Parse()

	// The credential CLI shares the -config/-configdir source selection; it
	// runs standalone and never boots the engine.
	if len(os.Args) > 1 && os.Args[1] == "cred" {
		os.Exit(credMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}

	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if err := checkConfigSource(explicit); err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		os.Exit(2)
	}

	startedAt := time.Now()

	lvl, err := llog.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		os.Exit(2)
	}
	log := slog.New(llog.NewTextHandler(os.Stderr, lvl))

	cfg, err := loadConfigSource(*cfgPath, *configDir, log)
	if err != nil {
		log.Error("config load failed", "err", err)
		os.Exit(1)
	}

	// plugins.dir may shadow any builtin Lua (loops, routes, support chunks)
	// by name — wire it before the first builtins.Resolve below.
	builtins.SetPluginDir(cfg.Plugins.Dir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Encrypted per-tenant credential store. Enabled via runtime.credentials;
	// the master key is read from the named env var (never the config file).
	// When disabled, credStore stays nil and http.request's `auth` fails with a
	// clear "not enabled" error instead of panicking. Opened before the memory
	// backends because their credentials resolve lazily against it.
	var credStore *credentials.Store
	if cfg.Runtime.Credentials.Enabled {
		envName := cfg.CredentialsMasterKeyEnv()
		masterKey := os.Getenv(envName)
		if masterKey == "" {
			log.Error("runtime.credentials.enabled but env var unset", "env", envName)
			os.Exit(1)
		}
		credStore, err = credentials.Open(cfg.CredentialsPath(), masterKey, log)
		if err != nil {
			log.Error("credential store open failed", "err", err)
			os.Exit(1)
		}
		defer credStore.Close()
	}

	// Lazy secret resolution: raw references (${VAR} / cred:<service>) in
	// registry secret fields resolve at consumer construction — env, then this
	// store, then skip-with-warning (optional components) or a clear
	// first-call error (model api_key). The legacy single-file path expands at
	// load, so its values are literals and pass straight through.
	credResolver := &config.Resolver{Store: credStore}

	// Outbound HTTP policy, shared by the Lua http.request op and the
	// builtin:fetch tool so the two cannot disagree. The guard lives in the
	// dialer — see internal/core/netguard — which is what makes it cover
	// redirect hops and DNS rebinding rather than just the URL as typed.
	netPolicy := netguard.Policy{AllowPrivate: cfg.Net.HTTP.AllowPrivate}
	if cfg.Net.HTTP.AllowPrivate {
		// Loud on purpose: this is the one setting that lets a prompt-injected
		// agent reach the host's own network, including the admin server and
		// any cloud metadata endpoint.
		log.Warn("net.http.allow_private is enabled: outbound HTTP may reach loopback, private, link-local and reserved addresses")
	}

	// Shell profile passwords may be lazy references; an unresolvable one is
	// cleared with a warning (the profile itself stays — docker needs no
	// password, ssh may still authenticate by key).
	for name, p := range cfg.Profiles.Shell {
		if p.Password == "" {
			continue
		}
		v, ok := credResolver.Resolve(ctx, p.Password)
		if !ok {
			log.Warn("shell profile credential unresolved; password cleared",
				"profile", name, "credential", config.CredentialName(p.Password))
			p.Password = ""
		} else {
			p.Password = v
		}
		cfg.Profiles.Shell[name] = p
	}

	// Memory backends. Pre-resolve default profiles so builtin:conversational
	// adds its default backend before we open the registry. Backend url /
	// password entries may be lazy secret references: an unresolvable one
	// skips the backend with a warning (honest degradation), never a boot
	// failure.
	memReg := memory.NewRegistry(log)
	memReg.RegisterProvider(sqlite.Provider{})
	memReg.RegisterProvider(redis.Provider{})
	memReg.RegisterProvider(mongodb.Provider{})
	memReg.RegisterProvider(postgres.Provider{})
	memReg.RegisterProvider(pgvector.Provider{})
	memReg.RegisterProvider(qdrant.Provider{})
	memReg.RegisterProvider(redisvector.Provider{})
	memReg.RegisterProvider(volatile.Provider{})
	for _, a := range cfg.Agents {
		_ = cfg.ResolveMemoryProfile(a)
	}
	// skippedBackends records backends dropped because a credential could not
	// be resolved; stores bound to them rebind to a survivor (see
	// memory.RebindSkipped) instead of failing the boot.
	skippedBackends := map[string]bool{}
	var memSurvivors []string
	for name, b := range cfg.Memory.Backends {
		cfg2, ok := resolveBackendSecrets(ctx, credResolver, name, b, log)
		if !ok {
			skippedBackends[name] = true
			continue // skipped: warning already logged
		}
		memReg.AddBackend(name, b.Provider, cfg2)
		memSurvivors = append(memSurvivors, name)
	}
	// Deterministic fallback choice: pickFallback prefers a text_search
	// survivor, but among equals the order must not depend on map iteration.
	sort.Strings(memSurvivors)
	if err := memReg.Open(ctx); err != nil {
		log.Error("memory backends failed", "err", err)
		os.Exit(1)
	}
	memMgr := memory.NewManager(memReg, log)

	// Drivers and shared infrastructure.
	llmMgr := llm.NewManager(cfg.Models, log)
	llmMgr.SetSecretResolver(func(raw string) (string, bool) {
		return credResolver.Resolve(ctx, raw)
	})
	opPool := pool.New(*workers)
	gw := gateway.NewRegistry(log)

	// Metrics registry + in-process history sampler. Created early so both the
	// terminal dashboard (stats panel) and the web console can read them; the
	// admin server mounts later. ~2h at 5s per counter, ring-bounded.
	metricReg := metrics.Global()
	history := metricReg.StartSampler(ctx, 5*time.Second, 1440)

	// Shell manager. Docker runs a single long-lived container per handle
	// (persistent fs/env/process state); SSH dials a remote host. Both are
	// generic providers; vendor-specific launchers live outside the runtime.
	shellMgr := shell.NewManager([]shell.ShellProvider{
		shell.NewDockerProvider(log),
		shell.NewSSHProvider(log),
	}, log)

	// Scheduler: session-owned timers. Fires are delivered as timer messages,
	// never by invoking a Luau state from a timer goroutine.
	schedSvc := scheduler.New(log)

	// Runtime store: persists timer/budget/child metadata.
	rtStore, err := runtime.Open(cfg.PersistencePath())
	if err != nil {
		log.Error("runtime store failed", "err", err)
		os.Exit(1)
	}

	// User-scoped file store: same blob-store family as media (fs or S3),
	// snapshot metadata in the runtime store's files_meta table. An
	// unresolvable S3 credential pair disables the store with a warning (the
	// media rule), never a boot failure; agents without the files capability
	// never touch it.
	var filesMgr *files.Manager
	if cfg.Files.Backend == "s3" {
		s3cfg, ok := resolveMediaS3(ctx, credResolver, cfg.Files.S3, log)
		if ok {
			blobStore, err := s3media.New(s3cfg)
			if err != nil {
				log.Error("files store failed", "err", err)
				os.Exit(1)
			}
			filesMgr = files.New(blobStore, rtStore, cfg.Files.FilesScratchTTL(), cfg.Files.FilesMaxBytes(), log)
		}
	} else {
		filesDir := cfg.Files.Dir
		if filesDir == "" {
			filesDir = filepath.Join(filepath.Dir(cfg.PersistencePath()), "files")
		}
		blobStore, err := media.Open(filesDir)
		if err != nil {
			log.Error("files store failed", "err", err)
			os.Exit(1)
		}
		filesMgr = files.New(blobStore, rtStore, cfg.Files.FilesScratchTTL(), cfg.Files.FilesMaxBytes(), log)
	}
	if filesMgr != nil {
		if n, err := filesMgr.SweepScratch(ctx); err != nil {
			log.Warn("files scratch sweep failed", "err", err)
		} else if n > 0 {
			log.Info("files scratch swept", "expired", n)
		}
	}

	// Tool registry: builtins + shell builtins + MCP discovery. The web_search
	// tool is backed by the configured search engines (config.Search); with no
	// engines it reports honest-unavailable.
	searchSet, err := search.Build(cfg.Search, credResolver, log)
	if err != nil {
		log.Error("search engines failed", "err", err)
		os.Exit(1)
	}
	legalSet, err := legal.Build(cfg.Legal)
	if err != nil {
		log.Error("legal engines failed", "err", err)
		os.Exit(1)
	}
	// The browser tool is opt-in and degrades rather than failing: no account
	// means an empty client and honest-unavailable, and a token that will not
	// resolve is skipped with a warning inside Build.
	browserClient := browser.Build(cfg.Browser, credResolver, log)
	toolReg := tools.NewRegistry()
	tools.RegisterBuiltins(toolReg, searchSet)
	tools.RegisterLegalBuiltins(toolReg, legalSet)
	tools.RegisterBrowserBuiltins(toolReg, browserClient)
	tools.RegisterFetchBuiltins(toolReg, fetch.New(netPolicy), log)
	tools.RegisterShellBuiltins(toolReg, shellMgr)
	mcpClients := map[string]*mcp.Client{}
	for sname, s := range cfg.MCP.Servers {
		c, err := mcp.NewClient(sname, s.Command, s.Args, log)
		if err != nil {
			log.Warn("mcp server failed", "server", sname, "err", err)
			continue
		}
		mcpClients[sname] = c
		mts, err := c.ListTools(ctx)
		if err != nil {
			log.Warn("mcp list tools failed", "server", sname, "err", err)
			continue
		}
		for _, t := range mts {
			toolReg.Register(t)
			log.Debug("mcp tool registered", "tool", t.Name)
		}
	}

	// Bake config-level tool spec overrides (tools.policy.overrides) into the
	// registry — after every Register* call (builtins, legal, shell, MCP) and
	// before any Expose. An override naming no registered tool is left for the
	// Lua path (it may target a tool.def-declared tool, which does not exist
	// until a chunk loads); a description referencing an unknown prompt key
	// still fails the boot, for either kind of tool.
	if err := toolReg.ApplyOverrides(cfg.Tools.Policy.Overrides, cfg.PromptTexts(), log); err != nil {
		log.Error("tool overrides failed", "err", err)
		os.Exit(1)
	}
	luaOverrides := tools.NewLuaOverrides(cfg.Tools.Policy.Overrides, toolReg.Names())
	// An override naming no registered tool is waiting on a loop to declare it.
	// Say so once at boot: the per-name warning only fires when a loop reports
	// its declared tools, which happens inside a work turn, so without this a
	// misspelling stays quiet until some agent is spoken to.
	if pending := luaOverrides.Pending(); len(pending) > 0 {
		log.Info("tools.policy.overrides: not registered; held for a Lua-declared tool",
			"tools", pending)
	}

	// Media blob store: one per process. Channels with a media policy land
	// inbound media here; llm caps resolve handles at request time. Backend
	// is fs (rooted beside the runtime persistence data) or s3. S3
	// credentials may be lazy references: unresolvable skips the store with a
	// warning (channels then run with media disabled), never a boot failure.
	var mediaStore media.Store
	if cfg.Media.Backend == "s3" {
		s3cfg, ok := resolveMediaS3(ctx, credResolver, cfg.Media.S3, log)
		if ok {
			mediaStore, err = s3media.New(s3cfg)
		}
	} else {
		mediaDir := cfg.Media.Dir
		if mediaDir == "" {
			mediaDir = filepath.Join(filepath.Dir(cfg.PersistencePath()), "media")
		}
		mediaStore, err = media.Open(mediaDir)
	}
	if err != nil {
		log.Error("media store failed", "err", err)
		os.Exit(1)
	}

	// Resolve per-agent memory and build handler maps.
	runtimeHandlers := caps.RuntimeHandlers(cfg.Triggers)
	// The resolved prompt registry (name -> text) is surfaced read-only to
	// every loop by agent.config() and is what {prompt: key} references
	// (instructions, tool descriptions) resolve against. One instance is
	// shared by every agent def, so the reload watcher refreshing a
	// file-backed entry is visible to all of them at once; the registry
	// serializes those writes against the reads agent.config() performs.
	promptReg := session.NewPromptRegistry(cfg.PromptTexts())
	promptFiles := cfg.PromptFiles()
	// Every agent's tool handlers share one LuaOverrides instance, so an
	// override is reported as unclaimed only when no loaded loop declares it.
	toolWiring := caps.ToolWiring{LuaOverrides: luaOverrides, Prompts: promptReg, Log: log}
	agentMemories := []memory.AgentMemory{}
	defs := map[string]*supervisor.AgentDef{}
	for name, a := range cfg.Agents {
		src, watchPath, err := builtins.Resolve(a.Loop)
		if err != nil {
			fields := []any{"agent", name, "err", err}
			if h := loopHint(a.Loop, toolReg.Names(), builtins.Names()); h != "" {
				fields = append(fields, "hint", h)
			}
			log.Error("agent loop resolve failed", fields...)
			os.Exit(1)
		}
		instrText, instrPrompt, err := resolveInstructions(a.Instructions, cfg.Prompts)
		if err != nil {
			log.Error("instructions read failed", "agent", name, "err", err)
			os.Exit(1)
		}
		instructions := &session.StringBox{}
		instructions.Store(instrText)

		var amPtr *memory.AgentMemory
		mp := cfg.ResolveMemoryProfile(a)
		if len(mp.Stores) > 0 {
			profile := map[string]memory.Store{}
			for sname, s := range mp.Stores {
				profile[sname] = memoryFromConfig(s)
			}
			// A store on a credential-skipped backend rebinds to a survivor
			// (or drops when none survives) — never a boot failure.
			profile, _ = memory.RebindSkipped(profile, skippedBackends, memSurvivors, memReg.Features, log)
			// Per-agent isolation: private stores are prefixed with the agent
			// name at bind time; stores opt into sharing with shared: true.
			am, err := memReg.ResolveStoresFor(name, profile)
			if err != nil {
				log.Error("memory resolve failed", "agent", name, "err", err)
				os.Exit(1)
			}
			am.Write = mp.Write
			am.Recall = mp.Recall
			am.EmbedModel = mp.EmbedModel
			am.RerankModel = mp.RerankModel
			am.Oversample = mp.Oversample
			agentMemories = append(agentMemories, am)
			amPtr = &am
		}

		effectiveCaps := capabilitySet(a.Capabilities)
		agentSet := toolReg.Expose(a.Skills, toolPolicyFor(cfg.Tools.Policy, effectiveCaps), false)
		// A skills entry naming a builtin loop matches no tool and is dropped
		// silently, so say so rather than letting the agent look toolless.
		for _, s := range a.Skills {
			if h := skillHint(s, builtins.Names()); h != "" {
				log.Warn("skills entry names a loop, not a tool", "agent", name, "skill", s, "hint", h)
			}
		}
		handlers := map[string]session.OpHandler{}
		enforce := cfg.Plugins.EnforceCaps()
		var withheld []string
		// Budget metering: if the agent declares tokens_per_day, wrap LLM
		// handlers with reserve/commit/release.
		llmHandlers := caps.LLMHandlers(llmMgr, mediaStore)
		if tokensPerDay := budgetTokens(a); tokensPerDay > 0 {
			pool := budget.NewPool(tokensPerDay)
			if w := budgetWindow(a); w > 0 {
				// Rolling-window budget: usage drains continuously as commits age
				// out, so there is no midnight cliff. Skip the daily reset.
				pool.SetWindow(w)
			} else {
				pool.StartDailyReset()
			}
			llmHandlers = caps.MeteredLLMHandlers(llmMgr, mediaStore, pool)
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "llm.chat", llmHandlers, &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "memory", caps.StoreHandlers(amPtr, memMgr), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "tools", caps.ToolHandlers(agentSet, toolWiring), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "shell.exec", caps.ShellHandlers(shellMgr), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "net.http", caps.HTTPHandlers(log, credStore, netPolicy), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "net.mail", caps.MailHandlers(log, credStore), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "files", caps.FileHandlers(filesMgr, name), &withheld) {
			handlers[k] = h
		}
		// runtime.* and credential.get stay ungated: the first is loop
		// machinery every agent needs, the second is already scoped by the
		// agent's own credential list.
		for k, h := range runtimeHandlers {
			handlers[k] = h
		}
		handlers["credential.get"] = caps.CredentialHandler(credStore, a.Credentials, log)
		if len(withheld) > 0 {
			log.Warn("capabilities withheld from agent", "agent", name, "ops", strings.Join(withheld, "; "))
		}

		canContact := stringSet(a.CanContact)
		safeDispatcher := resolveSafety(cfg, a.Safety)
		defs[name] = &supervisor.AgentDef{
			Info: &session.Info{
				Name:          name,
				Model:         a.Model,
				Instructions:  instructions,
				HistoryBudget: a.HistoryBudget,
				Memory:        amPtr,
				Shell:         shellProfileMap(cfg, a.Shell),
				Skills:        a.Skills,
				Capabilities:  a.Capabilities,

				InstructionsPath:   a.Instructions.File,
				InstructionsPrompt: instrPrompt,
				Prompts:            promptReg,
				Extras:             a.Extras,
				Credentials:        a.Credentials,
			},
			LoopFile:         watchPath,
			LoopSrc:          src,
			InstructionsPath: a.Instructions.File,
			Handlers:         handlers,
			CanContact:       canContact,
			Capabilities:     effectiveCaps,
			Safety:           safeDispatcher,
			Persistent:       a.Persistent,
		}
	}

	// Resolve spawn profiles into supervisor templates. A spawn profile's
	// grants are validated against the global ceiling at config load; the
	// supervisor attenuates them against the parent at spawn time.
	for pname, p := range cfg.Profiles.Agent {
		src, watchPath, err := builtins.Resolve(p.Loop)
		if err != nil {
			fields := []any{"profile", pname, "err", err}
			if h := loopHint(p.Loop, toolReg.Names(), builtins.Names()); h != "" {
				fields = append(fields, "hint", h)
			}
			log.Error("spawn profile loop resolve failed", fields...)
			os.Exit(1)
		}
		instrText, instrPrompt, err := resolveInstructions(p.Instructions, cfg.Prompts)
		if err != nil {
			log.Error("spawn profile instructions read failed", "profile", pname, "err", err)
			os.Exit(1)
		}
		instructions := &session.StringBox{}
		instructions.Store(instrText)

		var amPtr *memory.AgentMemory
		mp, hasProfile := cfg.Profiles.Memory[p.Memory]
		if p.Memory == "conversational" {
			mp, hasProfile = config.DefaultMemoryProfile(), true
		}
		if hasProfile {
			profile := map[string]memory.Store{}
			for sname, s := range mp.Stores {
				profile[sname] = memoryFromConfig(s)
			}
			// Same credential-skip degradation as the static-agent path above.
			profile, _ = memory.RebindSkipped(profile, skippedBackends, memSurvivors, memReg.Features, log)
			am, err := memReg.ResolveStoresFor("spawn:"+pname, profile)
			if err != nil {
				log.Error("spawn profile memory resolve failed", "profile", pname, "err", err)
				os.Exit(1)
			}
			am.Write = mp.Write
			am.Recall = mp.Recall
			am.EmbedModel = mp.EmbedModel
			am.RerankModel = mp.RerankModel
			am.Oversample = mp.Oversample
			agentMemories = append(agentMemories, am)
			amPtr = &am
		}

		profileCaps := capabilitySet(p.Capabilities)
		agentSet := toolReg.Expose(p.Skills, toolPolicyFor(cfg.Tools.Policy, profileCaps), false)
		for _, s := range p.Skills {
			if h := skillHint(s, builtins.Names()); h != "" {
				log.Warn("skills entry names a loop, not a tool", "profile", pname, "skill", s, "hint", h)
			}
		}
		handlers := map[string]session.OpHandler{}
		enforce := cfg.Plugins.EnforceCaps()
		var withheld []string
		// Budget metering for spawn profiles: a profile that declares
		// budget.tokens_per_day gets a metered LLM pool shared by every child
		// spawned from it — e.g. a manager variant or worker pool gets its
		// own budget. (Static agents each get their own pool above; drawing a
		// profile's pool from the parent's budget is a follow-up.)
		llmHandlers := caps.LLMHandlers(llmMgr, mediaStore)
		if p.Budget.TokensPerDay > 0 {
			pool := budget.NewPool(p.Budget.TokensPerDay)
			if w, err := time.ParseDuration(p.Budget.Window); err == nil && w > 0 {
				pool.SetWindow(w)
			} else {
				pool.StartDailyReset()
			}
			llmHandlers = caps.MeteredLLMHandlers(llmMgr, mediaStore, pool)
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "llm.chat", llmHandlers, &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "memory", caps.StoreHandlers(amPtr, memMgr), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "tools", caps.ToolHandlers(agentSet, toolWiring), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "shell.exec", caps.ShellHandlers(shellMgr), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "net.http", caps.HTTPHandlers(log, credStore, netPolicy), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "net.mail", caps.MailHandlers(log, credStore), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "files", caps.FileHandlers(filesMgr, pname), &withheld) {
			handlers[k] = h
		}
		for k, h := range runtimeHandlers {
			handlers[k] = h
		}
		handlers["credential.get"] = caps.CredentialHandler(credStore, p.Credentials, log)
		if len(withheld) > 0 {
			log.Warn("capabilities withheld from spawn profile", "profile", pname, "ops", strings.Join(withheld, "; "))
		}

		tmpl := &supervisor.SpawnTemplate{
			Name:         pname,
			LoopFile:     watchPath,
			LoopSrc:      src,
			Model:        p.Model,
			Instructions: p.Instructions.File,
			Shell:        shellProfileMap(cfg, p.Shell),
			CanContact:   stringSet(p.CanContact),
			Capabilities: profileCaps,
			Skills:       p.Skills,
			Handlers:     handlers,
			Safety:       resolveSafety(cfg, ""),
			Memory: &session.Info{
				Name:          "spawn:" + pname,
				Model:         p.Model,
				Instructions:  instructions,
				HistoryBudget: 6000,
				Memory:        amPtr,
				Skills:        p.Skills,
				Capabilities:  p.Capabilities,

				InstructionsPath:   p.Instructions.File,
				InstructionsPrompt: instrPrompt,
				Prompts:            promptReg,
				Extras:             p.Extras,
				Credentials:        p.Credentials,
			},
		}
		defs["__spawn__"+pname] = &supervisor.AgentDef{
			SpawnTemplate: tmpl,
			CanContact:    tmpl.CanContact,
			Capabilities:  tmpl.Capabilities,
			Handlers:      handlers,
		}
		// Also register the template under the spawned child's agent name
		// ("spawn:<name>") so a parent can deliver to an existing child by its
		// session:<id> address: Deliver resolves the agent name to a def, and a
		// live child at <agent>|<childKey> is found without re-spawning.
		childDef := &supervisor.AgentDef{
			SpawnTemplate: tmpl,
			Info:          tmpl.Memory,
			CanContact:    tmpl.CanContact,
			Capabilities:  tmpl.Capabilities,
			Handlers:      handlers,
			LoopFile:      watchPath,
			LoopSrc:       src,
		}
		defs["spawn:"+pname] = childDef
	}

	memMgr.StartGC(ctx, 5*time.Minute, agentMemories)

	// Terminal dashboard: only when attached to a TTY and not disabled. The tee
	// handler mirrors slog records into the TUI log pane and drops the stderr
	// copy (the TUI owns the screen). On non-TTY output (piped logs, systemd)
	// the plain stderr logger stays. The snapshot closure reads sup lazily so
	// the dashboard can be created before the supervisor.
	var sup *supervisor.Supervisor
	var dash *tui.Dashboard
	if !*noTUI && isatty.IsTerminal(os.Stdout.Fd()) {
		dash = tui.Start(tui.Source{
			Snapshot: func() ([]supervisor.SessionStatus, int, int) {
				if sup == nil {
					return nil, 0, 0
				}
				return sup.Snapshot()
			},
			Metrics: metricReg.Snapshot,
			Spark: func(name string) []int64 {
				pts := history.Series(name)
				out := make([]int64, len(pts))
				for i, p := range pts {
					out[i] = p.Value
				}
				return out
			},
		})
		log = slog.New(dash.LogHandler(llog.NewTextHandler(io.Discard, lvl)))
		slog.SetDefault(log)
		defer dash.Stop()
	}

	sup = supervisor.New(defs, gw, opPool, shellMgr, log)

	// Message journal (core-owned audit). Every inbound event is recorded at
	// the router and every egress at the session actor; loops and channels
	// can neither skip nor forge it. Journal errors are logged, never fatal.
	if cfg.Audit.AuditEnabled() {
		if days := cfg.Audit.AuditRetention(); days > 0 {
			cutoff := time.Now().AddDate(0, 0, -days)
			if n, err := rtStore.PruneMessages(ctx, cutoff); err != nil {
				log.Warn("journal prune failed", "err", err)
			} else if n > 0 {
				log.Info("journal pruned", "rows", n, "retention_days", days)
			}
			go func() {
				tick := time.NewTicker(24 * time.Hour)
				defer tick.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
						if _, err := rtStore.PruneMessages(ctx, time.Now().AddDate(0, 0, -days)); err != nil {
							log.Warn("journal prune failed", "err", err)
						}
					}
				}
			}()
		}
		sup.EgressJournal = func(rec session.EgressRecord) {
			errStr := rec.Err
			entry := runtime.JournalEntry{
				ID:          rec.InReplyTo,
				Direction:   "out",
				Status:      rec.Status,
				Channel:     rec.Channel,
				Chat:        rec.ReplyTo,
				Sender:      rec.ReplyTo,
				Agent:       rec.Agent,
				SessionID:   rec.SessionID,
				Type:        "agent",
				Text:        rec.Text,
				Attachments: rec.Attachments,
				Err:         errStr,
			}
			if entry.ID == "" {
				entry.ID = uuid.NewString()
			}
			if err := rtStore.RecordMessage(ctx, entry); err != nil {
				log.Warn("journal egress write failed", "err", err)
			}
		}
	}

	// Web-console log tail: mirror every record into a ring buffer so the web
	// UI's SSE endpoint can stream it. Wraps whichever handler is current
	// (plain stderr, or the TUI tee) so both views see the same lines.
	logRing := webui.NewLogRing(500)
	log = slog.New(webui.NewTeeHandler(log.Handler(), logRing))
	slog.SetDefault(log)

	sup.SetScheduler(schedSvc)
	sup.Start(ctx)

	// Router: routing is Lua (builtin:per_chat unless overridden).
	routeRef := cfg.Gateway.Route
	if routeRef == "" {
		routeRef = "plugin:per_chat"
	}
	routeSrc, _, err := builtins.Resolve(routeRef)
	if err != nil {
		log.Error("route plugin resolve failed", "err", err)
		os.Exit(1)
	}
	rtr := router.New(routeSrc, caps.TriggersResponse(cfg.Triggers), sup, log)
	if cfg.Audit.AuditEnabled() {
		rtr.Journal = func(in router.Inbound, status string) {
			msg := in.Message
			chat := ""
			if msg.Payload != nil {
				if c, ok := msg.Payload["chat_id"]; ok {
					chat = fmt.Sprint(c)
				}
			}
			var prov map[string]any
			if msg.Provenance != nil {
				if b, err := json.Marshal(msg.Provenance); err == nil {
					_ = json.Unmarshal(b, &prov)
				}
			}
			ts := msg.Ts
			if ts == 0 {
				ts = time.Now().Unix()
			}
			if err := rtStore.RecordMessage(ctx, runtime.JournalEntry{
				ID:          msg.ID,
				Ts:          ts,
				Direction:   "in",
				Status:      status,
				Channel:     in.Channel,
				Chat:        chat,
				Sender:      msg.From,
				Agent:       in.Agent,
				Type:        msg.Type,
				Text:        msg.Text,
				Attachments: msg.Attachments,
				Provenance:  prov,
			}); err != nil {
				log.Warn("journal ingress write failed", "err", err)
			}
		}
	}
	go rtr.Run(ctx)

	// Identity layer (opt-in). When enabled, every inbound channel event is
	// minted a stable user UUID before the router sees it, and the supervisor
	// gains a user resolver for session.push_user. When disabled, the sink is
	// the router directly — unchanged behavior.
	var identReg *identity.Registry
	var sink router.Sink = rtr
	if cfg.Runtime.Identity.Enabled {
		idPath := cfg.IdentityPath()
		if dir := filepath.Dir(idPath); dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0750)
		}
		identReg, err = identity.Open(idPath, log)
		if err != nil {
			log.Error("identity registry failed", "err", err)
			os.Exit(1)
		}
		sink = identity.NewSink(rtr, identReg, log)
		sup.SetUserResolver(identReg)
		log.Info("identity layer enabled", "db", idPath)
	}

	// Channels. All HTTP channels (webhook, ghhook, telegram-webhook/auto)
	// now attach to one shared httpd.Server on a single listener, instead of
	// each spinning up its own port. Two phases:
	//   1. construct drivers (this mounts every path on the mux),
	//   2. bind the listener, then Start the telegram drivers — because auto
	//      probes <public_url>/health, which round-trips through the public
	//      proxy back to this listener, and a probe before bind always fails.
	mediaPol := func(ch config.Channel) (media.Policy, bool) {
		if len(ch.Media.Allow) == 0 {
			return media.Policy{}, false
		}
		return media.Policy{MaxBytes: int64(ch.Media.MaxBytes), Allow: ch.Media.Allow}, true
	}

	httpdLog := log.With("module", "httpd")
	httpSrv := httpd.New(cfg.Gateway.Listen, httpdLog)
	var telegramDrivers []*telegram.Driver
	for i, ch := range cfg.Gateway.Channels {
		name := ch.Name
		if name == "" {
			name = fmt.Sprintf("%s-%d", ch.Type, i)
		}
		mpol, mok := mediaPol(ch)
		var mstore media.Store
		if mok {
			mstore = mediaStore
		}
		switch ch.Type {
		case "webhook":
			var wopts webhook.Options
			if ch.Timeout != "" {
				wopts.Timeout, _ = time.ParseDuration(ch.Timeout) // validated at config load
			}
			wopts.Async = ch.Async
			d := webhook.New(name, ch.Path, ch.Agent, sink, httpSrv, mstore, mpol, wopts, log)
			gw.Register(d)
		case "telegram":
			token, ok := credResolver.Resolve(ctx, ch.Token)
			if !ok {
				log.Warn("channel skipped: unresolved credential",
					"channel", name, "field", "token", "credential", config.CredentialName(ch.Token))
				continue
			}
			secretToken, ok := credResolver.Resolve(ctx, ch.SecretToken)
			if !ok {
				log.Warn("channel skipped: unresolved credential",
					"channel", name, "field", "secret_token", "credential", config.CredentialName(ch.SecretToken))
				continue
			}
			d := telegram.New(name, token, ch.Agent, ch.Mode, ch.AllowUsers, ch.Path, cfg.Gateway.PublicURL, secretToken, sink, httpSrv, mstore, mpol, log)
			gw.Register(d)
			telegramDrivers = append(telegramDrivers, d)
		case "ghhook":
			secret := ch.Secret
			if secret != "" {
				v, ok := credResolver.Resolve(ctx, secret)
				if !ok {
					log.Warn("channel skipped: unresolved credential",
						"channel", name, "field", "secret", "credential", config.CredentialName(secret))
					continue
				}
				secret = v
			}
			d := ghhook.New(name, ch.Path, ch.Agent, secret, sink, httpSrv, log)
			gw.Register(d)
		}
	}
	// Bind the shared listener now that every path is mounted, BEFORE the
	// telegram auto probe runs (a probe before bind would fail and force
	// polling). A bind failure (port in use, bad addr) is fatal and must
	// surface before "agentflow up".
	if err := httpSrv.Start(); err != nil {
		log.Error("httpd server failed to start", "listen", cfg.Gateway.Listen, "err", err)
		os.Exit(1)
	}
	for _, d := range telegramDrivers {
		if err := d.Start(ctx); err != nil {
			log.Error("channel start failed", "channel", d.Name(), "err", err)
			os.Exit(1)
		}
	}

	// The reload watcher also keeps file-backed prompt entries live: it polls
	// each backing file and republishes the text to the shared registry, and
	// updates in place the instructions of every agent sourced from that key.
	var promptSrc *reload.PromptSource
	if len(promptFiles) > 0 {
		promptSrc = &reload.PromptSource{Registry: promptReg, Files: promptFiles}
	}
	watcher := reload.New(sup, promptSrc, log)
	watcher.Start()

	// Metrics/admin: authenticated HTTP endpoint with health/readiness/metrics
	// and a read-only sessions view. Binds to loopback by default. The web
	// console (on by default, -no-webui to disable) mounts its SPA and JSON API
	// here and requires the bearer token on every API route — with no
	// ADMIN_TOKEN set, a per-boot token is generated and printed once.
	adminAddr := cfg.Runtime.Admin.Listen
	if adminAddr == "" {
		adminAddr = "127.0.0.1:9090"
	}
	adminToken := os.Getenv("ADMIN_TOKEN")
	if !*noWebUI && adminToken == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			log.Error("admin token generation failed", "err", err)
			os.Exit(1)
		}
		adminToken = hex.EncodeToString(b)
		log.Info("web console admin token (set ADMIN_TOKEN to pin it)", "token", adminToken)
	}
	admin := metrics.NewAdminServer(adminAddr, adminToken, metricReg, log)
	admin.SetReady(true)
	admin.SetSessions(func() []metrics.SessionInfo {
		infos := []metrics.SessionInfo{}
		for _, a := range sup.Agents() {
			if a.Info == nil {
				continue // __spawn__ templates carry no session Info
			}
			infos = append(infos, metrics.SessionInfo{Agent: a.Info.Name})
		}
		return infos
	})
	admin.SetCredentials(credStore)
	if !*noWebUI {
		console := webui.New(webui.Deps{
			ConfigPath: *cfgPath,
			Cfg:        cfg,
			Models:     llmMgr,
			History:    history,
			Creds:      credStore,
			Logs:       logRing,
			Version:    version,
			StartedAt:  startedAt,
			Snapshot: func() ([]supervisor.SessionStatus, int, int) {
				return sup.Snapshot()
			},
		})
		admin.Mount("/admin/api/", console.API(), true)
		// Docs site (embedded, unauthenticated). Redirect bare /docs so the
		// page's relative asset paths (css/, js/) resolve under the subtree.
		admin.Mount("/docs", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
		}), false)
		admin.Mount("/docs/", console.Docs(), false)
		admin.Mount("/", console.Static(), false)
		log.Info("web console enabled", "url", "http://"+adminAddr+"/")
	}
	go func() {
		if err := admin.Start(); err != nil {
			log.Warn("admin server stopped", "err", err)
		}
	}()

	// Engine trigger scheduler: every:/cron: triggers fire from the engine
	// itself — no agent holds the scheduler capability and no loop runs its own
	// calendar. A fire enqueues an ordinary Message{type:"cron"} (payload copied
	// from the trigger) into the target profile's session; run_on_boot fires once
	// at startup. Started here, after channels are registered, so a boot fire's
	// reply has somewhere to go.
	triggerSvc := triggers.New(
		func(profile string) (string, bool) { return triggerTarget(cfg, profile) },
		func(agent, key string, msg session.Message) error { return sup.Deliver(agent, key, msg) },
		cfg.Runtime.TimezoneOffset(),
		log,
	)
	triggerSvc.Start(ctx)
	triggerSvc.Reload(cfg.Triggers)
	// A configdir's triggers/*.yaml is re-read on a poll, so editing a schedule
	// needs no restart (the rest of the directory still does).
	if *configDir != "" {
		go triggerSvc.WatchConfigDir(ctx, *configDir)
	}

	// Daemon agents (persistent: true) get a synthetic boot message so their
	// sessions spawn now — after channels are registered, so a boot-turn reply
	// has somewhere to go — instead of waiting for first external traffic.
	sup.BootPersistent()

	log.Info("agentflow up", "agents", len(defs), "channels", len(cfg.Gateway.Channels))
	<-ctx.Done()

	log.Info("shutting down")
	watcher.Stop()
	memMgr.Stop()
	for _, c := range mcpClients {
		_ = c.Close()
	}
	_ = memReg.Close()
	_ = rtStore.Close()
	if identReg != nil {
		_ = identReg.Close()
	}
	adminStopCtx, adminCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer adminCancel()
	_ = admin.Stop(adminStopCtx)
	// The shared httpd.Server owns the one listener; stop it once here.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	httpSrv.Stop(shutdownCtx)
}

// checkConfigSource enforces -config / -configdir mutual exclusion. The
// default -config value is not "set" unless the operator passed it explicitly
// (tracked via flag.Visit), so `-configdir dir` alone is legal.
func checkConfigSource(explicit map[string]bool) error {
	if explicit["config"] && explicit["configdir"] {
		return fmt.Errorf("-config and -configdir are mutually exclusive")
	}
	return nil
}

// loadConfigSource loads the config from whichever source the flags select:
// a config directory (-configdir) or the single file (-config).
func loadConfigSource(cfgPath, configDir string, log *slog.Logger) (*config.Config, error) {
	if configDir != "" {
		return config.LoadDir(configDir, log)
	}
	return config.Load(cfgPath)
}

func capabilitySet(caps []string) map[string]bool {
	if len(caps) == 0 {
		caps = config.DefaultCapabilities
	}
	return stringSet(caps)
}

// gateOps restricts one op group to the capabilities the agent actually holds,
// appending what it withheld to *withheld so the boot notice reports what
// really happened rather than a hand-kept list that can drift from the wiring.
//
// enforce is plugins.enforce_capabilities; setting it false returns the map
// untouched, which is the migration escape for a configuration that relied on
// every agent having every op.
func gateOps(enforce bool, agent string, granted map[string]bool, capability string,
	hs map[string]session.OpHandler, withheld *[]string) map[string]session.OpHandler {
	if !enforce || granted[capability] {
		return hs
	}
	names := make([]string, 0, len(hs))
	for n := range hs {
		names = append(names, n)
	}
	sort.Strings(names)
	*withheld = append(*withheld, fmt.Sprintf("%s (needs %s)", strings.Join(names, ", "), capability))
	return caps.Gate(hs, capability, agent, granted)
}

// loopHint explains a loop: value that names something else, or "" when there
// is nothing to say.
//
// Two mistakes land here. A tool name produces "unknown plugin", which is true
// but says nothing about what to write instead. And a loop name in its
// pre-rename spelling ("builtin:per_chat") now names nothing at all, because
// that prefix was exactly the ambiguity the rename removed: it used to be
// spelled by loops, tools and memory providers alike, and nothing in the string
// said which one a name belonged to.
func loopHint(value string, toolNames, loopNames []string) string {
	if slices.Contains(toolNames, value) {
		return value + " is a tool name, not a loop — tools are listed under skills:, while loop: takes a plugin:<name> reference or a .lua path"
	}
	if name, ok := strings.CutPrefix(value, "builtin:"); ok && slices.Contains(loopNames, "plugin:"+name) {
		return "loops are spelled plugin: now, not builtin: — write plugin:" + name
	}
	return ""
}

// skillHint explains a skills: entry that names a loop or plugin instead of a
// tool, or "" when there is nothing to say.
//
// This one is worth catching because it fails silently: a loop name matches no
// registered tool, the entry is simply ignored, and the agent ends up with an
// empty tool list that looks like the tools being unavailable rather than
// misspelled.
//
// The reverse — a skills entry matching no registered tool — is deliberately
// not warned about, because a loop may legitimately list tools it declares
// itself with tool.def, which do not exist at boot.
func skillHint(value string, loopNames []string) string {
	if slices.Contains(loopNames, value) {
		return value + " is a loop/plugin name, not a tool — loops go in loop:, tools in skills:"
	}
	if name, ok := strings.CutPrefix(value, "builtin:"); ok && slices.Contains(loopNames, "plugin:"+name) {
		return "loops are spelled plugin: now, and never belong in skills: — did you mean plugin:" + name + " under loop:?"
	}
	return ""
}

// resolveInstructions sources an agent's or spawn profile's system prompt from
// either a file path or a prompts: registry key. It returns the text and the
// registry key ("" for a file source), which agent.config() surfaces as
// instructions_path vs instructions_prompt. An unreadable file or an unknown
// key is a boot error — a prompt never silently degrades to empty.
func resolveInstructions(ref config.InstructionsRef, prompts map[string]config.Prompt) (text, promptKey string, err error) {
	if ref.Prompt != "" {
		p, ok := prompts[ref.Prompt]
		if !ok {
			return "", "", fmt.Errorf("instructions references unknown prompt %q", ref.Prompt)
		}
		return p.Content, ref.Prompt, nil
	}
	if ref.File == "" {
		return "", "", nil
	}
	b, err := os.ReadFile(ref.File)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", ref.File, err)
	}
	return string(b), "", nil
}

// triggerTarget resolves a trigger's target.profile to the supervisor address
// that owns it: a configured agent keeps its name, a spawn profile becomes
// "spawn:<name>" (a top-level session running that profile's loop and grants).
// An unknown profile is not fatal — the trigger scheduler logs it and skips
// that trigger.
func triggerTarget(cfg *config.Config, profile string) (string, bool) {
	if profile == "" {
		return "", false
	}
	if _, ok := cfg.Agents[profile]; ok {
		return profile, true
	}
	if _, ok := cfg.Profiles.Agent[profile]; ok {
		return "spawn:" + profile, true
	}
	return "", false
}

// toolPolicyFor returns the tools policy for one agent or spawn profile. An
// agent without the shell.exec capability must not see the registry shell
// tools (builtin:shell.exec / shell.write / shell.destroy), even under a
// permissive tools policy — the same gate as shell profile binding at config
// load. The forbidden list is cloned before appending so the shared policy is
// never mutated.
func toolPolicyFor(policy config.ToolsPolicy, caps map[string]bool) config.ToolsPolicy {
	if caps["shell.exec"] {
		return policy
	}
	out := policy
	out.Forbidden = append(append([]string{}, policy.Forbidden...),
		"builtin:shell.exec", "builtin:shell.write", "builtin:shell.destroy")
	return out
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" {
			out[value] = true
		}
	}
	return out
}

// resolveSafety turns a safety profile reference into a dispatcher. "none"
// or "" means safety:none (explicit opt-out). "default" gives the builtin
// baseline. Unknown names are treated as none with a warning.
func resolveSafety(cfg *config.Config, ref string) *safety.Dispatcher {
	switch ref {
	case "", "none":
		return safety.New(safety.None)
	case "default":
		return safety.New(safety.DefaultProfile())
	default:
		return safety.New(safety.None)
	}
}

// budgetTokens extracts the tokens_per_day from the agent's budget config.
func budgetTokens(a config.Agent) int64 {
	if a.Budget == nil {
		return 0
	}
	v, ok := a.Budget["tokens_per_day"]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// budgetWindow extracts the optional rolling-window duration from the agent's
// budget config (e.g. budget: { tokens: N, window: "168h" }). Returns 0 when
// unset, which selects the daily-reset accounting mode.
func budgetWindow(a config.Agent) time.Duration {
	if a.Budget == nil {
		return 0
	}
	v, ok := a.Budget["window"]
	if !ok {
		return 0
	}
	s, ok := v.(string)
	if !ok {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

func memoryFromConfig(s config.Store) memory.Store {
	ret := memory.Store{
		Backend:    s.Backend,
		Table:      s.Table,
		Collection: s.Collection,
		Window:     s.Window,
		Requires:   s.Requires,
		Shared:     s.Shared,
	}
	if s.Retention != "" {
		d, err := time.ParseDuration(s.Retention)
		if err == nil {
			ret.Retention = d
		}
	}
	return ret
}

// resolveBackendSecrets resolves the secret-bearing entries (url, password)
// of one memory backend config. An unresolvable credential logs a warning
// naming it and reports ok=false — the backend is skipped, never a boot
// failure. The input map is cloned; the config struct is not mutated.
func resolveBackendSecrets(ctx context.Context, res *config.Resolver, name string, b config.Backend, log *slog.Logger) (map[string]any, bool) {
	if b.Config == nil {
		return nil, true
	}
	out := make(map[string]any, len(b.Config))
	for k, v := range b.Config {
		out[k] = v
	}
	for _, key := range []string{"url", "password", "api_key"} {
		raw, ok := out[key].(string)
		if !ok || raw == "" {
			continue
		}
		v, ok := res.Resolve(ctx, raw)
		if !ok {
			log.Warn("memory backend skipped: unresolved credential",
				"backend", name, "field", key, "credential", config.CredentialName(raw))
			return nil, false
		}
		out[key] = v
	}
	return out, true
}

// resolveMediaS3 resolves the S3 credential pair. Unresolvable logs a warning
// naming the credential and reports ok=false — the store is skipped.
func resolveMediaS3(ctx context.Context, res *config.Resolver, s3 config.MediaS3, log *slog.Logger) (config.MediaS3, bool) {
	out := s3
	for _, key := range []struct {
		field string
		dst   *string
	}{
		{"access_key", &out.AccessKey},
		{"secret_key", &out.SecretKey},
	} {
		if *key.dst == "" {
			continue
		}
		v, ok := res.Resolve(ctx, *key.dst)
		if !ok {
			log.Warn("media store skipped: unresolved credential",
				"field", key.field, "credential", config.CredentialName(*key.dst))
			return config.MediaS3{}, false
		}
		*key.dst = v
	}
	return out, true
}

func shellProfileMap(cfg *config.Config, name string) map[string]any {
	if name == "" || cfg.Profiles.Shell == nil {
		return nil
	}
	p, ok := cfg.Profiles.Shell[name]
	if !ok {
		return nil
	}
	return map[string]any{
		"provider":  p.Provider,
		"image":     p.Image,
		"workdir":   p.WorkDir,
		"network":   p.Network,
		"mem_limit": p.MemLimit,
		"cpu_limit": p.CPULimit,
		"env":       p.Env,
		"host":      p.Host,
		"user":      p.User,
		"password":  p.Password,
		"key_file":  p.KeyFile,
	}
}
