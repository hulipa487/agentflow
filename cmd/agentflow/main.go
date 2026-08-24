// agentflow — phase 2 entrypoint: memory, tools, MCP, router, channels, llm.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"agentflow/internal/builtins"
	"agentflow/internal/config"
	"agentflow/internal/core/budget"
	"agentflow/internal/core/caps"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/media"
	"agentflow/internal/core/memory"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/reload"
	"agentflow/internal/core/router"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/safety"
	"agentflow/internal/core/scheduler"
	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/core/tools"
	"agentflow/internal/drivers/ghhook"
	"agentflow/internal/drivers/httpd"
	"agentflow/internal/drivers/llm"
	"agentflow/internal/drivers/mcp"
	"agentflow/internal/drivers/mongodb"
	"agentflow/internal/drivers/pgvector"
	"agentflow/internal/drivers/postgres"
	"agentflow/internal/drivers/redis"
	"agentflow/internal/drivers/shell"
	"agentflow/internal/drivers/store"
	"agentflow/internal/drivers/store/volatile"
	"agentflow/internal/drivers/telegram"
	"agentflow/internal/drivers/webhook"
	"agentflow/internal/llog"
	"agentflow/internal/ui"
	"agentflow/internal/webui"
)

// version is stamped via -ldflags "-X main.version=..." on release builds.
var version = "dev"

func main() {
	cfgPath := flag.String("config", "agentflow.yaml", "path to agentflow.yaml")
	workers := flag.Int("workers", 8, "op worker pool size")
	logLevel := flag.String("log-level", "info", "minimum log level: dev|debug|info|warn|error (additive)")
	noTUI := flag.Bool("no-tui", false, "disable the terminal dashboard (plain stderr logs)")
	noWebUI := flag.Bool("no-webui", false, "disable the web console (admin server keeps token-optional loopback behavior)")
	flag.Parse()

	startedAt := time.Now()

	lvl, err := llog.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		os.Exit(2)
	}
	log := slog.New(llog.NewTextHandler(os.Stderr, lvl))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config load failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Memory backends. Pre-resolve default profiles so builtin:conversational
	// adds its default backend before we open the registry.
	memReg := memory.NewRegistry(log)
	memReg.RegisterProvider(store.Provider{})
	memReg.RegisterProvider(redis.Provider{})
	memReg.RegisterProvider(mongodb.Provider{})
	memReg.RegisterProvider(postgres.Provider{})
	memReg.RegisterProvider(pgvector.Provider{})
	memReg.RegisterProvider(volatile.Provider{})
	for _, a := range cfg.Agents {
		_ = cfg.ResolveMemoryProfile(a)
	}
	for name, b := range cfg.Memory.Backends {
		memReg.AddBackend(name, b.Provider, b.Config)
	}
	if err := memReg.Open(ctx); err != nil {
		log.Error("memory backends failed", "err", err)
		os.Exit(1)
	}
	memMgr := memory.NewManager(memReg, log)

	// Drivers and shared infrastructure.
	llmMgr := llm.NewManager(cfg.Models, log)
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

	// Encrypted per-tenant credential store. Enabled via runtime.credentials;
	// the master key is read from the named env var (never the config file).
	// When disabled, credStore stays nil and http.request's `auth` fails with a
	// clear "not enabled" error instead of panicking.
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

	// Scheduler: session-owned timers. Fires are delivered as timer messages,
	// never by invoking a Luau state from a timer goroutine.
	schedSvc := scheduler.New(log)

	// Runtime store: persists timer/budget/child metadata.
	rtStore, err := runtime.Open(cfg.PersistencePath())
	if err != nil {
		log.Error("runtime store failed", "err", err)
		os.Exit(1)
	}

	// Tool registry: builtins + shell builtins + MCP discovery.
	toolReg := tools.NewRegistry()
	tools.RegisterBuiltins(toolReg)
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

	// Media blob store: one per process, rooted beside the runtime
	// persistence data. Channels with a media policy land inbound media here;
	// llm caps resolve handles to inline base64 at request time.
	mediaDir := filepath.Join(filepath.Dir(cfg.PersistencePath()), "media")
	mediaStore, err := media.Open(mediaDir)
	if err != nil {
		log.Error("media store failed", "err", err)
		os.Exit(1)
	}

	// Resolve per-agent memory and build handler maps.
	agentMemories := []memory.AgentMemory{}
	defs := map[string]*supervisor.AgentDef{}
	for name, a := range cfg.Agents {
		src, watchPath, err := builtins.Resolve(a.Loop)
		if err != nil {
			log.Error("agent loop resolve failed", "agent", name, "err", err)
			os.Exit(1)
		}
		instructions := &session.StringBox{}
		if a.Instructions != "" {
			b, err := os.ReadFile(a.Instructions)
			if err != nil {
				log.Error("instructions read failed", "agent", name, "file", a.Instructions, "err", err)
				os.Exit(1)
			}
			instructions.Store(string(b))
		}

		var amPtr *memory.AgentMemory
		mp := cfg.ResolveMemoryProfile(a)
		if len(mp.Stores) > 0 {
			profile := map[string]memory.Store{}
			for sname, s := range mp.Stores {
				profile[sname] = memoryFromConfig(s)
			}
			am, err := memReg.ResolveStores(profile)
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

		agentSet := toolReg.Expose(a.Skills, cfg.Tools.Policy, false)
		handlers := map[string]session.OpHandler{}
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
		for k, h := range llmHandlers {
			handlers[k] = h
		}
		for k, h := range caps.StoreHandlers(amPtr, memMgr) {
			handlers[k] = h
		}
		for k, h := range caps.ToolHandlers(agentSet) {
			handlers[k] = h
		}
		for k, h := range caps.ShellHandlers(shellMgr) {
			handlers[k] = h
		}
		for k, h := range caps.HTTPHandlers(log, credStore) {
			handlers[k] = h
		}
		for k, h := range caps.MailHandlers(log, credStore) {
			handlers[k] = h
		}

		effectiveCaps := capabilitySet(a.Capabilities)
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
			},
			LoopFile:         watchPath,
			LoopSrc:          src,
			InstructionsPath: a.Instructions,
			Handlers:         handlers,
			CanContact:       canContact,
			Capabilities:     effectiveCaps,
			Safety:           safeDispatcher,
		}
	}

	// Resolve spawn profiles into supervisor templates. A spawn profile's
	// grants are validated against the global ceiling at config load; the
	// supervisor attenuates them against the parent at spawn time.
	for pname, p := range cfg.Profiles.Agent {
		src, watchPath, err := builtins.Resolve(p.Loop)
		if err != nil {
			log.Error("spawn profile loop resolve failed", "profile", pname, "err", err)
			os.Exit(1)
		}
		instructions := &session.StringBox{}
		if p.Instructions != "" {
			b, err := os.ReadFile(p.Instructions)
			if err != nil {
				log.Error("spawn profile instructions read failed", "profile", pname, "file", p.Instructions, "err", err)
				os.Exit(1)
			}
			instructions.Store(string(b))
		}

		var amPtr *memory.AgentMemory
		mp, hasProfile := cfg.Profiles.Memory[p.Memory]
		if p.Memory == "builtin:conversational" {
			mp, hasProfile = config.DefaultMemoryProfile(), true
		}
		if hasProfile {
			profile := map[string]memory.Store{}
			for sname, s := range mp.Stores {
				profile[sname] = memoryFromConfig(s)
			}
			am, err := memReg.ResolveStores(profile)
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

		agentSet := toolReg.Expose(p.Skills, cfg.Tools.Policy, false)
		handlers := map[string]session.OpHandler{}
		// Budget metering for spawn profiles: a profile that declares
		// budget.tokens_per_day gets a metered LLM pool shared by every child
		// spawned from it — e.g. a manager variant or worker pool gets its
		// own budget. (Static agents carve per-agent pools above; profile-
		// level carve-down from the parent's pool is a follow-up.)
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
		for k, h := range llmHandlers {
			handlers[k] = h
		}
		for k, h := range caps.StoreHandlers(amPtr, memMgr) {
			handlers[k] = h
		}
		for k, h := range caps.ToolHandlers(agentSet) {
			handlers[k] = h
		}
		for k, h := range caps.ShellHandlers(shellMgr) {
			handlers[k] = h
		}
		for k, h := range caps.HTTPHandlers(log, credStore) {
			handlers[k] = h
		}
		for k, h := range caps.MailHandlers(log, credStore) {
			handlers[k] = h
		}

		tmpl := &supervisor.SpawnTemplate{
			Name:         pname,
			LoopFile:     watchPath,
			LoopSrc:      src,
			Model:        p.Model,
			Instructions: p.Instructions,
			Shell:        shellProfileMap(cfg, p.Shell),
			CanContact:   stringSet(p.CanContact),
			Capabilities: capabilitySet(p.Capabilities),
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
	var dash *ui.Dashboard
	if !*noTUI && isatty.IsTerminal(os.Stdout.Fd()) {
		dash = ui.Start(ui.Source{
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
		routeRef = "builtin:per_chat"
	}
	routeSrc, _, err := builtins.Resolve(routeRef)
	if err != nil {
		log.Error("route plugin resolve failed", "err", err)
		os.Exit(1)
	}
	rtr := router.New(routeSrc, sup, log)
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
		var mstore *media.Store
		if mok {
			mstore = mediaStore
		}
		switch ch.Type {
		case "webhook":
			d := webhook.New(name, ch.Path, ch.Agent, sink, httpSrv, mstore, mpol, log)
			gw.Register(d)
		case "telegram":
			d := telegram.New(name, ch.Token, ch.Agent, ch.Mode, ch.AllowUsers, ch.Path, cfg.Gateway.PublicURL, sink, httpSrv, mstore, mpol, log)
			gw.Register(d)
			telegramDrivers = append(telegramDrivers, d)
		case "ghhook":
			d := ghhook.New(name, ch.Path, ch.Agent, ch.Secret, sink, httpSrv, log)
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

	watcher := reload.New(sup, log)
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
		admin.Mount("/", console.Static(), false)
		log.Info("web console enabled", "url", "http://"+adminAddr+"/")
	}
	go func() {
		if err := admin.Start(); err != nil {
			log.Warn("admin server stopped", "err", err)
		}
	}()

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

func capabilitySet(caps []string) map[string]bool {
	if len(caps) == 0 {
		caps = config.DefaultCapabilities
	}
	return stringSet(caps)
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
	}
	if s.Retention != "" {
		d, err := time.ParseDuration(s.Retention)
		if err == nil {
			ret.Retention = d
		}
	}
	return ret
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
