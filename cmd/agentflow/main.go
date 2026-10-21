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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	stdruntime "runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/mattn/go-isatty"

	"agentflow/internal/builtins"

	"agentflow/internal/config"
	"agentflow/internal/core/accounting"
	"agentflow/internal/core/budget"
	"agentflow/internal/core/caps"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/files"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/inbox"
	"agentflow/internal/core/lease"
	"agentflow/internal/core/limits"
	"agentflow/internal/core/media"
	"agentflow/internal/core/memory"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/moderation"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/oidc"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/reload"
	"agentflow/internal/core/router"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/safety"
	"agentflow/internal/core/scheduler"
	"agentflow/internal/core/session"
	"agentflow/internal/core/sessionhub"
	"agentflow/internal/core/storedb"
	"agentflow/internal/core/studio"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/core/tenancy"
	"agentflow/internal/core/tools"
	"agentflow/internal/core/triggers"
	"agentflow/internal/core/users"
	"agentflow/internal/drivers/bridge"
	"agentflow/internal/drivers/browser"
	"agentflow/internal/drivers/fetch"
	"agentflow/internal/drivers/ghhook"
	"agentflow/internal/drivers/httpd"
	"agentflow/internal/drivers/legal"
	"agentflow/internal/drivers/llm"
	"agentflow/internal/drivers/logfile"
	"agentflow/internal/drivers/mcp"
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
	"agentflow/internal/vm"
	"agentflow/internal/webui"
)

// version is stamped via -ldflags "-X main.version=..." on release builds.
var version = "dev"

func main() {
	// Standalone commands (version, validate, doctor, init, config schema,
	// loop check, tools import-openapi, cred) never boot the engine. The
	// no-subcommand path below is unchanged.
	if handled, code := runSubcommand(hoistConfigFlags(os.Args[1:]), os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}

	cfgPath := flag.String("config", "", "config file or directory (default: agentflow.yaml, then config.yaml)")
	configDir := flag.String("configdir", "", "deprecated: config directory (use -config DIR)")
	workers := flag.Int("workers", 8, "op worker pool size (overrides runtime.scheduler.workers)")
	logLevel := flag.String("log-level", "info", "minimum log level: dev|debug|info|warn|error (additive)")
	logFormat := flag.String("log-format", "", "log format: text|json (default text)")
	logJSON := flag.Bool("log-json", false, "deprecated: use -log-format json")
	logFile := flag.String("log-file", "", "append logs to this file as well as stderr")
	noTUI := flag.Bool("no-tui", false, "disable the terminal dashboard (plain stderr logs)")
	noConsole := flag.Bool("no-console", false, "serve the admin API without the static web console")
	noWebUI := flag.Bool("no-webui", false, "deprecated: use -no-console")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() { printUsage(os.Stderr) }
	flag.Parse()

	if *showVersion {
		fmt.Printf("agentflow %s\nprelude %d\ngo %s\n", version, vm.PreludeVersion, stdruntime.Version())
		return
	}
	// A positional argument is almost always a subcommand typo or a stray
	// shell argument. Refusing it is safer than silently booting an engine the
	// caller did not mean to start.
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "agentflow: unexpected argument %q\nrun `agentflow help` for commands and examples\n", flag.Arg(0))
		os.Exit(2)
	}

	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	startedAt := time.Now()

	lvl, err := llog.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		os.Exit(2)
	}
	var logOut io.Writer = os.Stderr
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "agentflow: log file:", err)
			os.Exit(1)
		}
		defer f.Close()
		logOut = io.MultiWriter(os.Stderr, f)
	}
	format := strings.ToLower(strings.TrimSpace(*logFormat))
	if format == "" {
		format = "text"
		if *logJSON {
			format = "json"
		}
	}
	if *logJSON && *logFormat != "" && format != "json" {
		fmt.Fprintln(os.Stderr, "agentflow: -log-json conflicts with -log-format", *logFormat)
		os.Exit(2)
	}
	var log *slog.Logger
	switch format {
	case "text":
		log = slog.New(llog.NewTextHandler(logOut, lvl))
	case "json":
		log = slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: lvl}))
	default:
		fmt.Fprintln(os.Stderr, "agentflow: -log-format must be text or json")
		os.Exit(2)
	}

	cfgSource, err := config.ResolveSource(*cfgPath, *configDir)
	if err != nil {
		log.Error("config source", "err", err)
		os.Exit(2)
	}
	if explicit["configdir"] {
		log.Warn("-configdir is deprecated; use -config DIR")
	}
	if explicit["no-webui"] {
		log.Warn("-no-webui is deprecated; use -no-console (the admin and Studio APIs stay available)")
	}
	consoleEnabled := !*noConsole && !*noWebUI
	if cfgSource.Legacy {
		log.Warn("using config.yaml; rename it to agentflow.yaml so both the engine and the CLI discover the same file")
	}
	cfg, err := cfgSource.Load(log)
	if err != nil {
		log.Error("config load failed", "err", err)
		os.Exit(1)
	}

	// The configuration's epoch: a fleet compares these to tell a stale or
	// divergent instance from a healthy one.
	log.Info("config loaded", "source", cfgSource.String(), "epoch", cfg.Epoch, "version", cfg.Version)
	if len(cfg.Agents) == 0 {
		log.Warn("no agents configured; starting as an admin/Studio control plane (create an agent in Studio or add one to the config)")
	}

	// plugins.dir may shadow any builtin Lua (loops, routes, support chunks)
	// by name — wire it before the first builtins.Resolve below.
	builtins.SetPluginDir(cfg.Plugins.Dir)
	builtins.SetPackages(cfg.Packages)

	// Every session loads the support chunks into its Lua state before its loop,
	// so they are policy exactly as a loop is, and a chunk this core cannot
	// serve is refused at boot for the same reason a loop is: the deployment
	// stops here, loudly, instead of coming up and failing session by session
	// against an API its Lua was not written for. See builtins.SupportChunks.
	if _, err := builtins.SupportChunks(); err != nil {
		log.Error("support chunk refused", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The dashboard reads stdin for keys, so it sees a ctrl+c typed into a
	// pseudo-terminal (Git Bash/mintty, some IDE terminals) even though Windows
	// never turns that keystroke into a console control event. Without the TUI
	// nothing reads stdin; watch it so the same ctrl+c still stops the engine.
	// A real console is unaffected: its ctrl+c arrives as SIGINT, not a byte.
	tuiActive := !*noTUI && isatty.IsTerminal(os.Stdout.Fd())
	if !tuiActive {
		watchStdinInterrupt(stop)
	}

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
		credStore, err = credentials.Open(cfg.CredentialsStore(), masterKey, log)
		if err != nil {
			log.Error("credential store open failed", "err", err)
			os.Exit(1)
		}
		defer credStore.Close()
		if !cfg.Runtime.Credentials.EngineWideMaintenance() {
			log.Warn("credential.get() exposes engine-wide secrets to ordinary turns; set runtime.credentials.engine_wide: maintenance to restrict it to engine-fired contexts")
		}
	}

	// Lazy secret resolution: raw references (${VAR} / cred:<service>) in
	// registry secret fields resolve at consumer construction — env, then this
	// store, then skip-with-warning (optional components) or a clear
	// first-call error (model api_key). The legacy single-file path expands at
	// load, so its values are literals and pass straight through.
	credResolver := &config.Resolver{Store: credStore}

	// hasCred answers credential existence for the user surface without ever
	// touching a value: a loop may ask whether its user holds a key, never what
	// the key is.
	hasCred := func(userID, service string) bool {
		if credStore == nil {
			return false
		}
		refs, err := credStore.List(context.Background(), userID)
		if err != nil {
			return false
		}
		for _, r := range refs {
			if r.Service == service {
				return true
			}
		}
		return false
	}

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
	warnUnenforceableGC(cfg, log)
	warnInertConfig(cfg, log)

	// Drivers and shared infrastructure.
	llmMgr := llm.NewManager(cfg.Models, log)
	llmMgr.SetSecretResolver(func(raw string) (string, bool) {
		return credResolver.Resolve(ctx, raw)
	})
	// runtime.scheduler.workers and -workers are one setting with one
	// precedence rule: the flag wins when given, otherwise the config decides,
	// otherwise the flag's default (8) stands.
	workerCount := *workers
	if !explicit["workers"] {
		if w := cfg.Runtime.Scheduler.Workers; w != nil && *w > 0 {
			workerCount = *w
		}
	}
	if workerCount <= 0 {
		log.Error("worker pool size must be positive", "value", workerCount)
		os.Exit(1)
	}
	log.Info("op pool", "workers", workerCount)
	opPool := pool.New(workerCount)
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

	// Runtime store: the message journal, the token ledger and its per-call
	// detail, and the engine's key/value rows — file trees, session state, route
	// state, shell handles.
	//
	// Scheduler timers are NOT here: the timer service is in-memory, so a
	// restart forgets every every:/after:/cron: timer until the owning loop
	// re-registers it. (This comment used to claim timer, budget and child
	// metadata; those tables — timer_meta, child_meta, budget_usage — are no
	// longer created by anything. They are left in place rather than dropped,
	// because a boot-time DROP against an operator's database is not
	// recoverable.)
	rtStore, err := runtime.OpenStore(cfg.PersistencePath(), log)
	if err != nil {
		log.Error("runtime store failed", "err", err)
		os.Exit(1)
	}
	// The tenancy registry: groups, memberships, pools, grants. It lives in
	// the runtime store because it is core operational state — the memory
	// scope layer enforces what it records, so it must exist wherever the
	// engine does (§8: pools + ACL, groups).
	tenReg := tenancy.New(rtStore, log)
	memReg.SetPoolResolver(func(name string) (string, error) {
		return tenReg.PoolUUIDByName(context.Background(), name)
	}) // Per-call token detail in the ledger (the daily rollup is always written).
	usageEvents := cfg.Usage.UsageEvents()

	// The log plane: the two append-only planes — the message journal and the
	// per-call usage detail — written constantly, read rarely, expired by age.
	// Empty keeps them in the runtime store, which is what every deployment had
	// before the plane could be configured; a directory puts them in an append
	// log instead (internal/drivers/logfile).
	var logPlane *logfile.Log
	if dir := cfg.LogPlaneDir(); dir != "" {
		lp, err := logfile.Open(dir)
		if err != nil {
			log.Error("log plane failed", "err", err)
			os.Exit(1)
		}
		defer lp.Close()
		logPlane = lp
		log.Info("log plane enabled", "backend", "file", "dir", dir)
	}
	// journalSink is where the audit trail goes, eventLog where the per-call
	// detail goes, and usageWriter is what the metering path calls: the rollup
	// stays in the transactional store — a quota check reads it before every
	// call — while the detail follows the plane.
	var journalSink runtime.Journal = rtStore
	var eventLog runtime.EventLog = rtStore
	var usageWriter caps.UsageRecorder = rtStore
	if logPlane != nil {
		journalSink, eventLog = logPlane, logPlane
		usageWriter = splitUsage{rollups: rtStore, events: logPlane}
	}

	// Singleton arbitration. Scheduled work — every:/cron: triggers, the file
	// GC, the retention prunes — has to run once per deployment, not once per
	// instance, and the lease is what decides which instance runs each piece. It
	// lives in the same store as everything else, so a deployment that shares a
	// database shares the arbitration for free, and one that runs alone is
	// uncontended.
	leaseMgr, err := lease.Open(cfg.ClusterStore(), lease.OwnerID(), lease.DefaultTTL, log)
	if err != nil {
		log.Error("lease manager failed", "err", err)
		os.Exit(1)
	}
	defer leaseMgr.Close()
	log.Info("instance identity", "owner", leaseMgr.Owner(), "region", cfg.Runtime.Region)

	// Two topology smells that are otherwise silent, because leases arbitrate
	// which instance owns a piece of work and a lease store no peer can see
	// makes every instance believe it is alone: singleton work runs once per
	// instance instead of once per deployment, and with a region set, an
	// instance in another region takes a session that is already running and
	// starts a second copy of the conversation.
	clusterTarget := cfg.ClusterStore()
	if storedb.BackendFor(clusterTarget) != storedb.BackendFor(cfg.PersistencePath()) {
		log.Warn("the lease store is on a different backend from the runtime store: singleton work will run once per instance rather than once per deployment",
			"lease_store", storedb.Display(clusterTarget))
	}
	if cfg.Runtime.Region != "" && storedb.BackendFor(clusterTarget) == storedb.BackendSQLite {
		log.Warn("runtime.region is set but the lease store is a local file: an instance in another region cannot see this one's sessions and will start a second copy of the conversation",
			"region", cfg.Runtime.Region)
	}

	// Every metric this instance exposes says which instance — and which region
	// — it came from. A scrape of N instances is otherwise N indistinguishable
	// series, and the instance id here is the one the logs and the lease table
	// use, so a dashboard label leads back to the process that produced it.
	labels := map[string]string{"instance": leaseMgr.Owner()}
	if cfg.Runtime.Region != "" {
		labels["region"] = cfg.Runtime.Region
	}
	if err := metricReg.SetLabels(labels); err != nil {
		log.Warn("metric labels not set", "err", err)
	}

	// Shell handle registry. A handle is a resource out on a host — a container
	// the session can reach from any instance — not a structure in this process,
	// so it is recorded where the fleet can see it: that is what lets the
	// instance that takes a session over adopt the shell the session was using,
	// and what lets a handle whose instance was killed be reclaimed rather than
	// living on the host forever.
	shellMgr.SetRegistry(caps.ShellStore{Store: rtStore}, leaseMgr.Owner())

	// singleton runs fn only while this instance holds the named lease. A store
	// that cannot answer is a store that cannot arbitrate: the work is skipped
	// rather than run by everyone.
	singleton := func(ctx context.Context, name string, fn func()) {
		ok, err := leaseMgr.Acquire(ctx, name)
		if err != nil {
			log.Warn("scheduled work skipped: lease unavailable", "name", name, "err", err)
			return
		}
		if !ok {
			log.Debug("scheduled work skipped: another instance holds it", "name", name)
			return
		}
		fn()
	}

	// agentDayUsage reads an agent's deployment-wide spend for today from the
	// token ledger. It is the figure a daily budget pool enforces against: a
	// pool counting only its own process would hand the full budget to every
	// instance, and forgive the day's spend whenever one restarted.
	agentDayUsage := func(agent string) func(context.Context) (int64, error) {
		return func(ctx context.Context) (int64, error) {
			t, err := rtStore.UsageForAgentDay(agent, runtime.DayKey(time.Now()))
			if err != nil {
				return 0, err
			}
			return t.Billable(), nil
		}
	}

	// Identity registry (opt-in). Opened here rather than with the rest of the
	// identity wiring below, because the per-user quota the LLM handlers
	// enforce resolves its limits from profiles.
	var identReg *identity.Registry
	if cfg.Runtime.Identity.Enabled {
		identReg, err = identity.Open(cfg.IdentityStore(), log)
		if err != nil {
			log.Error("identity registry failed", "err", err)
			os.Exit(1)
		}
		// An unlinked handle carries no personal scope unless the deployment
		// opts back into claiming a profile on first contact.
		identReg.SetAutoClaim(!cfg.Runtime.Users.RegistrationRequired())

		// Channel traits, keyed by the name drivers report. The built-in
		// defaults are per driver *type*, and drivers report the configured
		// channel name — so a deployment that names its channels (the normal
		// case) would treat telegram as unverifiable without this.
		channelTraits := map[string]identity.ChannelTraits{}
		for _, ch := range cfg.Gateway.Channels {
			name := ch.Name
			if name == "" {
				name = ch.Type
			}
			channelTraits[name] = identity.Traits(ch.Type)
		}
		if err := identReg.SetChannelTraits(channelTraits); err != nil {
			log.Error("identity channel traits failed", "err", err)
			os.Exit(1)
		}

		// Mixed backends are legal, and usually a mistake worth saying out loud:
		// an instance whose identity store is shared but whose journal is local
		// has a fleet-wide view of who people are and a single-instance view of
		// what they said.
		if got, want := storedb.BackendFor(cfg.IdentityStore()), storedb.BackendFor(cfg.PersistencePath()); got != want {
			log.Warn("identity store and runtime store are on different backends",
				"identity", got, "runtime", want)
		}
	}

	// Per-user token quota: durable (read from the ledger, so a restart
	// forgives nothing) and only meaningful for a linked handle — traffic with
	// no profile has no account to charge.
	var userQuota *accounting.Quota
	if identReg != nil {
		userQuota = accounting.New(rtStore, identReg.LimitFor, cfg.Usage.DefaultTokensPerDay)
	}

	// The loop-visible projection of a profile: a display name and the handles,
	// never an email or a secret. The conversion lives here because caps cannot
	// import identity — the router's tests import caps, and identity imports
	// router, so that edge would close a cycle.
	profileFor := func(userID string) (caps.LoopProfile, bool, error) {
		if identReg == nil {
			return caps.LoopProfile{}, false, nil
		}
		p, ok, err := identReg.Get(userID)
		if err != nil || !ok {
			return caps.LoopProfile{}, false, err
		}
		return loopViewOf(p), true, nil
	}
	profilesFor := func() ([]caps.LoopProfile, error) {
		if identReg == nil {
			return nil, nil
		}
		ps, err := identReg.List()
		if err != nil {
			return nil, err
		}
		out := make([]caps.LoopProfile, 0, len(ps))
		for _, p := range ps {
			out = append(out, loopViewOf(p))
		}
		return out, nil
	}

	// User-scoped file store: same blob-store family as media (fs or S3),
	// snapshot metadata in the runtime store's files_meta table. An
	// unresolvable S3 credential pair disables the store with a warning (the
	// media rule), never a boot failure; agents without the files capability
	// never touch it.
	var filesMgr *files.Manager
	if cfg.Files.Backend == "s3" {
		s3cfg, ok := resolveMediaS3(ctx, credResolver, cfg.Files.S3, "files", log)
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
			filesDir = filepath.Join(cfg.DataDir(), "files")
		}
		blobStore, err := media.Open(filesDir)
		if err != nil {
			log.Error("files store failed", "err", err)
			os.Exit(1)
		}
		filesMgr = files.New(blobStore, rtStore, cfg.Files.FilesScratchTTL(), cfg.Files.FilesMaxBytes(), log)
	}
	if filesMgr != nil {
		singleton(ctx, "files-scratch", func() {
			if n, err := filesMgr.SweepScratch(ctx); err != nil {
				log.Warn("files scratch sweep failed", "err", err)
			} else if n > 0 {
				metrics.Add("agentflow_files_scratch_swept", int64(n))
				log.Info("files scratch swept", "expired", n)
			}
		})
		// Blob GC: mark-and-sweep unreferenced blobs past the grace window.
		// Disabled with files.gc_grace: "0". One instance sweeps: two concurrent
		// mark-and-sweeps over one blob store can delete what the other is still
		// writing.
		if grace := cfg.Files.FilesGCGrace(); grace > 0 {
			singleton(ctx, "files-gc", func() {
				if live, swept, err := filesMgr.GC(ctx, grace); err != nil {
					log.Warn("files blob gc failed", "err", err)
				} else if swept > 0 {
					metrics.Add("agentflow_files_gc_swept", int64(swept))
					log.Info("files blob gc swept", "live", live, "swept", swept)
				}
			})
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
	tools.RegisterBrowserBuiltins(toolReg, browserClient, filesMgr)
	tools.RegisterFetchBuiltins(toolReg, fetch.New(netPolicy), log, filesMgr)
	tools.RegisterShellBuiltins(toolReg, shellMgr)
	// Declarative HTTP tools: config-declared REST integrations, no Go code per
	// API. They share the same dialer guard and per-tenant credential store as
	// http.request and builtin:fetch.
	tools.RegisterHTTPTools(toolReg, cfg.Tools.HTTP, credStore, netPolicy, log)
	mcpClients := map[string]*mcp.Client{}
	for sname, s := range cfg.MCP.Servers {
		// url selects the Streamable HTTP transport, command the stdio one;
		// config validation already rejects a server declaring neither. A
		// server declaring both is a config mistake rather than a real
		// either/or, so it is called out rather than silently resolved.
		var c *mcp.Client
		var err error
		switch {
		case s.URL != "":
			if s.Command != "" {
				log.Warn("mcp server declares both url and command; using the url (Streamable HTTP)", "server", sname)
			}
			token := s.Token
			if token != "" {
				v, ok := credResolver.Resolve(ctx, token)
				if !ok {
					log.Warn("mcp server skipped: unresolved credential",
						"server", sname, "field", "token", "credential", config.CredentialName(token))
					continue
				}
				token = v
			}
			// An operator-configured loopback endpoint (a local sidecar) is
			// trusted the same way the webhook's own config is; remote URLs
			// keep the shared address guard. This preserves the local MCP
			// setup that predates the guard without reopening private-network
			// destinations to a model-driven tool.
			policy := netPolicy
			if u, perr := url.Parse(s.URL); perr == nil && hostIsLoopback(u.Hostname()) {
				policy = netguard.Policy{AllowPrivate: true}
			}
			c, err = mcp.NewHTTPClientWithPolicy(sname, s.URL, token, policy, log)
		default:
			c, err = mcp.NewClient(sname, s.Command, s.Args, log)
		}
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
		s3cfg, ok := resolveMediaS3(ctx, credResolver, cfg.Media.S3, "media", log)
		if ok {
			mediaStore, err = s3media.New(s3cfg)
		}
	} else {
		mediaDir := cfg.Media.Dir
		if mediaDir == "" {
			mediaDir = filepath.Join(cfg.DataDir(), "media")
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
	// Per-tenant egress rate limit (E18): netguard restricts where an agent
	// may connect, this restricts how often. Keyed on the personal uuid — the
	// tenant — and shared by every agent's handlers in the process.
	egressRate := &limits.Rate{Limit: cfg.Runtime.Limits.EgressPerMinute, Window: time.Minute}
	limitEgress := func(h session.OpHandler) session.OpHandler {
		if egressRate.Limit <= 0 {
			return h
		}
		return func(ctx context.Context, op session.Op) (string, bool) {
			if !egressRate.Allow(session.BillingUUIDFromCtx(ctx)) {
				b, _ := json.Marshal("egress rate limit exceeded for this tenant; retry after the minute rolls over")
				return string(b), false
			}
			return h(ctx, op)
		}
	}
	// Model-backed safety classifiers are built once and shared by every
	// agent profile that references them. Each client owns its cache, breaker
	// and concurrency limit; the policy (thresholds, label actions) is applied
	// per agent by the safety filter.
	classifierClients := map[string]*moderation.Client{}
	for cname, p := range cfg.Safety.Classifiers {
		prompt := ""
		if p.Prompt != "" {
			prompt = cfg.Prompts[p.Prompt].Content
		}
		labels := make([]string, 0, len(p.Categories))
		for label := range p.Categories {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		var modPool *budget.Pool
		if p.Budget.TokensPerDay > 0 {
			modPool = budget.NewPool(p.Budget.TokensPerDay)
			// Moderation has its own ledger bucket, so its spend never drains
			// an agent's tokens_per_day pool or a user's LLM quota.
			modPool.SetUsageSource(agentDayUsage("__moderation_" + cname))
			modPool.StartDailyReset()
		}
		concurrency := p.MaxConcurrency
		if concurrency == 0 {
			concurrency = 8
		}
		client, err := moderation.New(llmMgr, moderation.Options{
			Name:           cname,
			Model:          p.Model,
			Prompt:         prompt,
			Labels:         labels,
			MaxTokens:      p.MaxTokens,
			MaxConcurrency: concurrency,
			CacheSize:      p.Cache.Size,
			CacheTTL:       p.CacheTTLD(),
			Pool:           modPool,
			Ledger:         usageWriter,
			Events:         usageEvents,
			LedgerAgent:    "__moderation_" + cname,
			Log:            log,
		})
		if err != nil {
			log.Error("safety classifier failed", "classifier", cname, "err", err)
			os.Exit(1)
		}
		classifierClients[cname] = client
	}

	// buildAgentDef resolves one configured agent into a supervisor
	// definition. It is the single recipe for agent construction: the boot
	// loop and the runtime registry (PUT /admin/api/agents/{name}, via the
	// webui's BuildAgent) both go through it, so an agent installed live is
	// built exactly like a boot one.
	buildAgentDef := func(name string, a config.Agent) (*supervisor.AgentDef, error) {
		src, watchPath, err := builtins.Resolve(a.Loop)
		if err != nil {
			if h := loopHint(a.Loop, toolReg.Names(), builtins.Names()); h != "" {
				return nil, fmt.Errorf("loop %q: %w (hint: %s)", a.Loop, err, h)
			}
			return nil, fmt.Errorf("loop %q: %w", a.Loop, err)
		}
		instrText, instrPrompt, err := resolveInstructions(a.Instructions, cfg.Prompts)
		if err != nil {
			return nil, fmt.Errorf("instructions: %w", err)
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
				return nil, fmt.Errorf("memory resolve: %w", err)
			}
			am.Write = mp.Write
			am.Recall = mp.Recall
			am.EmbedModel = mp.EmbedModel
			am.RerankModel = mp.RerankModel
			am.Oversample = mp.Oversample
			agentMemories = append(agentMemories, am)
			amPtr = &am
		}

		// The model must exist where this builder's caller resolves models:
		// the live manager covers both boot (seeded from models:) and the
		// runtime registry (extended by model upserts). Like validate(), the
		// check applies only to an agent that names a model — one that does
		// not resolves "default" per call, and fails there if it is missing.
		if a.Model != "" {
			if _, err := llmMgr.Get(a.Model); err != nil {
				return nil, err
			}
		}

		effectiveCaps := capabilitySet(a.Capabilities)
		agentSet := toolReg.Expose(a.Skills, toolPolicyFor(cfg.Tools.Policy, effectiveCaps), false).
			RequireCapabilities(effectiveCaps)
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
		// LLM handlers are always metered, so every call reaches the per-user
		// ledger; a declared tokens_per_day additionally turns the pool into a
		// hard budget enforced before the call.
		var pool *budget.Pool
		if tokensPerDay := budgetTokens(a); tokensPerDay > 0 {
			pool = budget.NewPool(tokensPerDay)
			if w := budgetWindow(a); w > 0 {
				// Rolling-window budget: usage drains continuously as commits age
				// out, so there is no midnight cliff. Skip the daily reset — and
				// the ledger, whose day-granular rows cannot express a window.
				pool.SetWindow(w)
			} else {
				// A daily budget belongs to the agent, not to this process.
				pool.SetUsageSource(agentDayUsage(name))
				pool.StartDailyReset()
			}
		}
		llmHandlers := caps.MeteredLLMHandlers(llmMgr, mediaStore, caps.Metering{
			Pool:   pool,
			Quota:  userQuota,
			Agent:  name,
			Ledger: usageWriter,
			Events: usageEvents,
			Log:    log,
		})
		for k, h := range gateOps(enforce, name, effectiveCaps, "llm.chat", llmHandlers, &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "memory", caps.StoreHandlers(amPtr, memMgr, tenReg), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "tools", caps.ToolHandlers(agentSet, toolWiring), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "shell.exec", caps.ShellHandlers(shellMgr, filesMgr, name), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "net.http", caps.HTTPHandlers(log, credStore, netPolicy, filesMgr), &withheld) {
			handlers[k] = limitEgress(h)
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "net.mail", caps.MailHandlers(log, credStore), &withheld) {
			handlers[k] = limitEgress(h)
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "files", caps.FileHandlers(filesMgr, name), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "session.state", caps.SessionStateHandlers{Store: rtStore}.Handlers(), &withheld) {
			handlers[k] = h
		}
		// The user surface: read-only facts about the sender, plus
		// maintenance-only profile lookup for the agent's own loops.
		userHandlers := caps.UserHandlers{
			Profile:       profileFor,
			Profiles:      profilesFor,
			Ledger:        rtStore,
			Quota:         userQuota,
			HasCredential: hasCred,
		}.Handlers()
		for k, h := range gateOps(enforce, name, effectiveCaps, "users", userHandlers, &withheld) {
			handlers[k] = h
		}
		// runtime.* and credential.get stay ungated: the first is loop
		// machinery every agent needs, the second is already scoped by the
		// agent's own credential list.
		for k, h := range runtimeHandlers {
			handlers[k] = h
		}
		handlers["credential.get"] = caps.CredentialHandlerPolicy(credStore, a.Credentials, cfg.Runtime.Credentials.EngineWideMaintenance(), log)
		if len(withheld) > 0 {
			log.Warn("capabilities withheld from agent", "agent", name, "ops", strings.Join(withheld, "; "))
		}

		canContact := stringSet(a.CanContact)
		safeDispatcher, err := resolveSafety(cfg, a.Safety, classifierClients, name)
		if err != nil {
			return nil, err
		}
		return &supervisor.AgentDef{
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
				InheritUser:        a.InheritUser,
			},
			LoopFile:         watchPath,
			LoopSrc:          src,
			InstructionsPath: a.Instructions.File,
			Handlers:         handlers,
			CanContact:       canContact,
			Capabilities:     effectiveCaps,
			Safety:           safeDispatcher,
			Persistent:       a.Persistent,
		}, nil
	}
	defs := map[string]*supervisor.AgentDef{}
	for name, a := range cfg.Agents {
		def, err := buildAgentDef(name, a)
		if err != nil {
			log.Error("agent build failed", "agent", name, "err", err)
			os.Exit(1)
		}
		defs[name] = def
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
		agentSet := toolReg.Expose(p.Skills, toolPolicyFor(cfg.Tools.Policy, profileCaps), false).
			RequireCapabilities(profileCaps)
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
		var pool *budget.Pool
		if p.Budget.TokensPerDay > 0 {
			pool = budget.NewPool(p.Budget.TokensPerDay)
			if w, err := time.ParseDuration(p.Budget.Window); err == nil && w > 0 {
				pool.SetWindow(w)
			} else {
				// The profile's budget, summed over every child every instance
				// has spawned — the ledger attributes a child's calls to the
				// profile it came from.
				pool.SetUsageSource(agentDayUsage(pname))
				pool.StartDailyReset()
			}
		}
		llmHandlers := caps.MeteredLLMHandlers(llmMgr, mediaStore, caps.Metering{
			Pool:   pool,
			Quota:  userQuota,
			Agent:  pname,
			Ledger: usageWriter,
			Events: usageEvents,
			Log:    log,
		})
		for k, h := range gateOps(enforce, pname, profileCaps, "llm.chat", llmHandlers, &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "memory", caps.StoreHandlers(amPtr, memMgr, tenReg), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "tools", caps.ToolHandlers(agentSet, toolWiring), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "shell.exec", caps.ShellHandlers(shellMgr, filesMgr, pname), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "net.http", caps.HTTPHandlers(log, credStore, netPolicy, filesMgr), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "net.mail", caps.MailHandlers(log, credStore), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "files", caps.FileHandlers(filesMgr, pname), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "session.state", caps.SessionStateHandlers{Store: rtStore}.Handlers(), &withheld) {
			handlers[k] = h
		}
		userHandlers := caps.UserHandlers{
			Profile:       profileFor,
			Profiles:      profilesFor,
			Ledger:        rtStore,
			Quota:         userQuota,
			HasCredential: hasCred,
		}.Handlers()
		for k, h := range gateOps(enforce, pname, profileCaps, "users", userHandlers, &withheld) {
			handlers[k] = h
		}
		for k, h := range runtimeHandlers {
			handlers[k] = h
		}
		handlers["credential.get"] = caps.CredentialHandlerPolicy(credStore, p.Credentials, cfg.Runtime.Credentials.EngineWideMaintenance(), log)
		if len(withheld) > 0 {
			log.Warn("capabilities withheld from spawn profile", "profile", pname, "ops", strings.Join(withheld, "; "))
		}

		safeDispatcher, err := resolveSafety(cfg, p.Safety, classifierClients, pname)
		if err != nil {
			log.Error("spawn profile safety resolution failed", "profile", pname, "err", err)
			os.Exit(1)
		}
		tmpl := &supervisor.SpawnTemplate{
			Name:          pname,
			LoopFile:      watchPath,
			LoopSrc:       src,
			Model:         p.Model,
			Instructions:  p.Instructions.File,
			Shell:         shellProfileMap(cfg, p.Shell),
			CanContact:    stringSet(p.CanContact),
			Capabilities:  profileCaps,
			Skills:        p.Skills,
			Handlers:      handlers,
			Safety:        safeDispatcher,
			InheritSafety: p.Safety == "",
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
				InheritUser:        p.InheritUser,
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
	if tuiActive {
		dash = tui.Start(tui.Source{
			// Bubble Tea owns the terminal in raw mode, so ctrl+c is a key
			// event here, not SIGINT. Cancel the engine context with the
			// dashboard; stop() also restores the default signal behavior so
			// a second ctrl+c can force-kill a stubborn shutdown.
			Quit: stop,
			Snapshot: func() (int, int) {
				if sup == nil {
					return 0, 0
				}
				// The dashboard shows the live counts only; per-session rows
				// are the admin API's job.
				_, active, idle := sup.Snapshot()
				return active, idle
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
	sup.SetPushSameTenant(cfg.Runtime.Users.PushSameTenant)

	// Studio (the standalone builder/playground app): one broker both observes
	// every session (trace rings) and acts as the synthetic "studio" channel,
	// so a playground reply arrives through the ordinary egress path — safety,
	// journaling and delivery unchanged.
	var studioBroker *studio.Broker
	var studioWS *studio.Workspace
	var channelsStore *studio.Channels
	var safetyStore *studio.Safety
	if cfg.Runtime.Studio.IsEnabled() {
		studioBroker = studio.NewBroker(sup, journalSink, studio.Config{
			TraceEvents:   cfg.Runtime.Studio.TraceEvents,
			TraceSessions: cfg.Runtime.Studio.TraceSessions,
			ChatTimeout:   cfg.Runtime.Studio.Timeout(),
			ValidateUser: func(id string) bool {
				if identReg == nil {
					return false
				}
				_, ok, err := identReg.Get(id)
				return err == nil && ok
			},
		}, log)
		gw.Register(studioBroker)
		sup.SetTracer(studioBroker.Trace, cfg.Runtime.Studio.TracePayloads)
		studioWS = studio.NewWorkspace(cfgSource.ConfigDir(), cfgSource.ConfigPath(), cfg.Runtime.Studio.Workspace, cfg, log)
		channelsStore = studio.NewChannels(cfg, cfgSource.ConfigDir(), cfgSource.ConfigPath(), func() []string {
			names := make([]string, 0, len(sup.Agents()))
			for name, def := range sup.Agents() {
				if def.SpawnTemplate != nil {
					continue
				}
				names = append(names, name)
			}
			return names
		}, log)
		safetyStore = studio.NewSafety(cfg, cfgSource.ConfigDir(), cfgSource.ConfigPath(), log)
	}
	// Per-tenant resource bounds (E18). Both are opt-in: 0 disables. The op
	// pool's fairness, the sharpest bound, is not here — it is how the pool
	// works now, not a setting.
	sup.SetSessionCap(cfg.Runtime.Limits.SessionsPerTenant)
	if mem, err := parseByteSize(cfg.Runtime.VM.MemoryLimit); err != nil {
		log.Error("runtime.vm.memory_limit is not a size", "value", *cfg.Runtime.VM.MemoryLimit, "err", err)
		os.Exit(1)
	} else if mem > 0 {
		sup.SetVMMemory(mem)
	}

	// Session hub. A conversation runs on one instance at a time, and in a
	// fleet any instance may receive its traffic — a webhook load-balanced
	// across the fleet, a trigger firing wherever its lease landed, a push from
	// another node. The hub puts a per-session lease in front of delivery and
	// the shared inbox behind it: an instance that owns the session (or can
	// take it) delivers the message itself, and one that does not queues it for
	// the owner. Without a shared store both are local, the lease is
	// uncontended, and this behaves exactly as it did before.
	hubQueue, err := inbox.Open(cfg.ClusterStore(), leaseMgr.Owner(), log)
	if err != nil {
		log.Error("session inbox failed", "err", err)
		os.Exit(1)
	}
	defer hubQueue.Close()
	hub := sessionhub.New(leaseMgr, hubQueue, sup.DeliverLocal, log)
	// A store in another region makes the hub's two periods a deployment's
	// business: the poll is the added latency a message pays when it arrives on
	// the wrong instance, and the TTL is how long a killed instance's sessions
	// stay unavailable. Unset, the hub's own defaults (250ms, 30s) stand.
	if d := cfg.Runtime.Cluster.ClusterPollInterval(); d > 0 {
		hub.SetPoll(d)
	}
	if d := cfg.Runtime.Cluster.ClusterSessionTTL(); d > 0 {
		hub.SetSessionTTL(d)
	}
	sup.SetHub(hub)
	// Group-identity validation: the router proposes, the engine resolves.
	// A proposal naming a group the sender is not active in is refused at the
	// deliver boundary; an accepted one stamps the derived membership uuid
	// onto the message's core-owned provenance (§5.7, A1).
	sup.SetMembershipResolver(func(personalUUID, groupUUID string) (string, error) {
		member, err := tenReg.IsMember(context.Background(), groupUUID, personalUUID)
		if err != nil {
			return "", err
		}
		if !member {
			return "", fmt.Errorf("%s is not an active member of group %s", personalUUID, groupUUID)
		}
		return tenancy.MembershipUUID(personalUUID, groupUUID), nil
	})
	go hub.Drain(ctx)

	// Shell handle reclaim. A session that ends reaps its own shells, but an
	// instance that is killed never runs that, and its containers would stay on
	// the host for good. One instance per deployment sweeps for them: a handle
	// whose session is alive nowhere, and which no live session has used for the
	// grace window, is released. A handle whose session is alive is left alone —
	// and has its clock refreshed, which is what makes the window a grace period
	// for a failover rather than a countdown from creation.
	reclaim := func() {
		n, err := shellMgr.Sweep(ctx, shell.ReclaimGrace, hub.Live)
		if err != nil {
			log.Warn("shell reclaim failed", "err", err)
			return
		}
		if n > 0 {
			log.Info("shell handles reclaimed", "handles", n)
		}
	}
	singleton(ctx, "shell-reclaim", reclaim)
	go func() {
		tick := time.NewTicker(shell.ReclaimInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				singleton(ctx, "shell-reclaim", reclaim)
			}
		}
	}()

	// Expired key/value rows — a route handler's state, the file store's
	// scratch — are reclaimed once a day. A row's deadline is a promise the
	// store keeps on read; this is what keeps the space, including for rows
	// nothing reads again.
	sweepRows := func() {
		if n, err := rtStore.SweepExpired(ctx); err != nil {
			log.Warn("expired row sweep failed", "err", err)
		} else if n > 0 {
			log.Info("expired rows reclaimed", "rows", n)
		}
		// Tenancy cascade: groups whose disband retention elapsed are gone
		// (SweepExpired above); their memberships, reverse indexes, pools and
		// grants follow (B7x).
		if err := tenReg.Sweep(ctx); err != nil {
			log.Warn("tenancy sweep failed", "err", err)
		}
	}
	singleton(ctx, "rows-sweep", sweepRows)
	go func() {
		tick := time.NewTicker(24 * time.Hour)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				singleton(ctx, "rows-sweep", sweepRows)
			}
		}
	}()

	// Token ledger retention: the daily rollup and (when enabled) the per-call
	// detail log age out on a daily tick, like the journal. One instance does it:
	// the delete is idempotent, but N instances deleting the same rows
	// concurrently is N times the work for no benefit.
	if days := cfg.Usage.UsageRetention(); days > 0 {
		prune := func() {
			cutoff := time.Now().AddDate(0, 0, -days)
			if err := rtStore.PruneUsage(cutoff); err != nil {
				log.Warn("usage prune failed", "err", err)
			}
			// The per-call detail may live on the log plane, where the rollup
			// prune above cannot reach it: retention is per plane.
			if logPlane != nil {
				if n, err := logPlane.PruneEvents(cutoff); err != nil {
					log.Warn("usage event prune failed", "err", err)
				} else if n > 0 {
					log.Info("usage events pruned", "rows", n, "retention_days", days)
				}
			}
		}
		singleton(ctx, "usage-prune", prune)
		go func() {
			tick := time.NewTicker(24 * time.Hour)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					singleton(ctx, "usage-prune", prune)
				}
			}
		}()
	}

	// Message journal (core-owned audit). Every inbound event is recorded at
	// the router and every egress at the session actor; loops and channels
	// can neither skip nor forge it. Journal errors are logged, never fatal.
	if cfg.Audit.AuditEnabled() {
		if days := cfg.Audit.AuditRetention(); days > 0 {
			prune := func() {
				cutoff := time.Now().AddDate(0, 0, -days)
				if n, err := journalSink.PruneMessages(ctx, cutoff); err != nil {
					log.Warn("journal prune failed", "err", err)
				} else if n > 0 {
					log.Info("journal pruned", "rows", n, "retention_days", days)
				}
			}
			singleton(ctx, "journal-prune", prune)
			go func() {
				tick := time.NewTicker(24 * time.Hour)
				defer tick.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
						singleton(ctx, "journal-prune", prune)
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
				UserUUID:    rec.UserUUID,
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
			if err := journalSink.RecordMessage(ctx, entry); err != nil {
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
	// The second value is the route's hot-reload watch path: the plugins.dir
	// shadow or file behind the ref, "" for an unshadowed builtin (the embedded
	// handler has no file to poll). It is kept and handed to the reload watcher
	// below — routing is policy, and policy is hot-reloadable here.
	routeSrc, routeWatchPath, err := builtins.Resolve(routeRef)
	if err != nil {
		log.Error("route plugin resolve failed", "err", err)
		os.Exit(1)
	}
	rtr := router.New(routeSrc, caps.TriggersResponse(cfg.Triggers), sup, log)
	// Route handlers get durable, deployment-wide state. A handler's Lua state
	// is per process, so this is where remembering anything belongs: the
	// routing decision a chat reads is the same on every instance, and it
	// survives a restart.
	rtr.SetStateStore(rtStore)
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
			// The identity sink stamps the profile behind the message on the
			// core-owned provenance; an unregistered handle stamps an empty one,
			// which is what the audit view needs to tell personal traffic from
			// anonymous traffic. Read from the provenance, never from the payload:
			// a hop's payload is rebuilt from the caller's Lua table.
			userUUID := ""
			if msg.Provenance != nil && msg.Provenance.UserUUID != nil {
				userUUID = *msg.Provenance.UserUUID
			}
			if err := journalSink.RecordMessage(ctx, runtime.JournalEntry{
				ID:          msg.ID,
				Ts:          ts,
				Direction:   "in",
				Status:      status,
				Channel:     in.Channel,
				Chat:        chat,
				Sender:      msg.From,
				UserUUID:    userUUID,
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
	// resolved to an identity before the router sees it, and the supervisor
	// gains a user resolver for session.push_user. The registry itself was
	// opened earlier (the quota needs it); this wires the sink. When disabled,
	// the sink is the router directly — unchanged behavior.
	var sink router.Sink = rtr
	if identReg != nil {
		sc := identity.NewSink(rtr, identReg, log)
		// A link attempt is answered where the user made it: the sink sends
		// the confirmation in-channel through the same gateway replies use.
		sc.SetReply(func(channel, replyTo, text string) {
			if err := gw.Send(channel, replyTo, text, nil); err != nil {
				log.Warn("identity reply failed", "channel", channel, "err", err)
			}
		})
		sc.SetRejectUnregistered(cfg.Runtime.Users.RejectUnregistered())
		sink = sc
		sup.SetUserResolver(identReg)
		// Per-user overrides: a profile's model and instruction layer reach every
		// session through agent.info(), so a loop written against the documented
		// idiom honours them without a line of Lua changing. Cached for a short
		// window, which is also how fast a change made on one instance reaches
		// the others.
		sup.SetProfileSettings(identity.NewSettingsSource(identReg, 0))
		// storedb.Display, not the raw target: in a fleet this is a DSN, and a
		// DSN carries a password.
		log.Info("identity layer enabled", "store", storedb.Display(cfg.IdentityStore()),
			"require_registration", cfg.Runtime.Users.RegistrationRequired())
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

	// User-facing profile API (opt-in): registration and channel linking on
	// the same public listener the HTTP channels attach to. Off by default —
	// the surface is public, so opening it is a deliberate act.
	if cfg.Runtime.Users.Enabled {
		if identReg == nil {
			// Config validation already rejects this pairing; the check stays
			// so a future refactor cannot mount a dead endpoint.
			log.Error("users API requires runtime.identity.enabled")
			os.Exit(1)
		}
		// Identity provider (opt-in): the engine verifies access tokens rather
		// than logging anyone in. An unreachable issuer fails the boot — a
		// misconfigured provider should not look like "every login is wrong".
		var oidcVerifier *oidc.Verifier
		if oc := cfg.Runtime.Users.OIDC; oc != nil {
			v, err := oidc.New(oidc.Config{
				Issuer:       oc.Issuer,
				Audience:     oc.Audience,
				JWKSURL:      oc.JWKSURL,
				SubjectClaim: oc.SubjectClaim,
				EmailClaim:   oc.EmailClaim,
				NameClaim:    oc.NameClaim,
			}, log)
			if err != nil {
				log.Error("identity provider unusable", "err", err, "issuer", oc.Issuer)
				os.Exit(1)
			}
			oidcVerifier = v
			log.Info("identity provider enabled", "issuer", oc.Issuer,
				"audience", oc.Audience, "jit_provisioning", cfg.Runtime.Users.JITEnabled())
		}
		httpSrv.Handle(users.Prefix+"/", users.New(identReg, cfg.Runtime.Users, log, users.Options{
			LinkableChannels: identReg.LinkableChannels(),
			Files:            filesMgr,
			Store:            rtStore,
			Quota:            userQuota,
			Verifier:         oidcVerifier,
			JITProvisioning:  cfg.Runtime.Users.JITEnabled(),
		}).Handler().ServeHTTP)
		log.Info("user API enabled", "prefix", users.Prefix,
			"registration", cfg.Runtime.Users.RegistrationMode(),
			"linkable", identReg.LinkableChannels(),
			"cors_origins", len(cfg.Runtime.Users.CORSOrigins))
	}
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
			d := webhook.New(name, ch.Path, ch.Agent, sink, httpSrv, mstore, mpol, wopts, netPolicy, log)
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
		case "bridge":
			secret, ok := credResolver.Resolve(ctx, ch.Secret)
			if !ok {
				log.Warn("channel skipped: unresolved credential",
					"channel", name, "field", "secret", "credential", config.CredentialName(ch.Secret))
				continue
			}
			token := ch.Token
			if token != "" {
				if v, ok := credResolver.Resolve(ctx, token); ok {
					token = v
				} else {
					log.Warn("bridge channel outbound token unresolved; delivering without auth",
						"channel", name, "credential", config.CredentialName(token))
					token = ""
				}
			}
			var timeout time.Duration
			if ch.Timeout != "" {
				timeout, _ = time.ParseDuration(ch.Timeout) // validated at config load
			}
			d := bridge.New(name, ch.Agent, bridge.Options{
				Path:       ch.Path,
				DeliverURL: ch.DeliverURL,
				Secret:     secret,
				Token:      token,
				Timeout:    timeout,
			}, sink, httpSrv, mstore, mpol, log)
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
	// The route handler and the plugins.dir support chunks are policy too, and
	// they are watched on the same terms as a loop: the route is swapped by
	// rebuilding the router's service state (no session is involved — routing
	// does not run in one), and a support chunk restarts every live session,
	// because every session loads all of them.
	watcher.SetRoute(&reload.RouteSource{Ref: routeRef, WatchPath: routeWatchPath, Apply: rtr.Reload})
	watcher.SetSupport(&reload.SupportSource{Paths: builtins.SupportChunkPaths(), Reload: sup.ReloadAll})
	watcher.SetPackages(&reload.PackageSource{Paths: builtins.PackagePaths(), Reload: sup.ReloadAll})
	watcher.Start()

	// Metrics/admin: authenticated HTTP endpoint with health/readiness/metrics
	// and a read-only sessions view. Binds to loopback by default.
	adminAddr := cfg.Runtime.Admin.Listen
	if adminAddr == "" {
		adminAddr = "127.0.0.1:9090"
	}
	adminSocket := strings.TrimSpace(cfg.Runtime.Admin.Socket)
	if adminSocket != "" && stdruntime.GOOS != "windows" && !filepath.IsAbs(adminSocket) {
		adminSocket = filepath.Join(cfgSource.Dir, adminSocket)
	}
	tcpAdmin := cfg.Runtime.Admin.TCPEnabled()
	authListen := adminAddr
	if !tcpAdmin {
		// A socket-only admin plane is local by construction; the loopback
		// sentinel keeps the non-loopback token guard from firing on a
		// disabled TCP address.
		authListen = "127.0.0.1:9090"
	}
	adminToken, tokErr := resolveAdminTokenWithOperators(os.Getenv("ADMIN_TOKEN"), authListen, len(cfg.Runtime.Admin.Operators) > 0, log)
	if tokErr != nil {
		log.Error("admin plane has no usable authentication", "err", tokErr)
		os.Exit(1)
	}
	admin := metrics.NewAdminServer(adminAddr, adminToken, metricReg, log)
	admin.SetCORS(cfg.Runtime.Admin.CORSOrigins)
	if len(cfg.Runtime.Admin.Operators) > 0 {
		ops := make(map[string]string, len(cfg.Runtime.Admin.Operators))
		for name, raw := range cfg.Runtime.Admin.Operators {
			v, ok := credResolver.Resolve(ctx, raw)
			if !ok {
				log.Warn("admin operator skipped: unresolved credential", "operator", name)
				continue
			}
			ops[name] = v
		}
		admin.SetOperators(ops)
	}
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
	{
		console := webui.New(webui.Deps{
			ConfigPath: cfgSource.ConfigPath(),
			ConfigDir:  cfgSource.ConfigDir(),
			Cfg:        cfg,
			Instance:   leaseMgr.Owner(),
			Region:     cfg.Runtime.Region,
			Models:     llmMgr,
			History:    history,
			Creds:      credStore,
			Logs:       logRing,
			Version:    version,
			StartedAt:  startedAt,
			Snapshot: func() ([]supervisor.SessionStatus, int, int) {
				return sup.Snapshot()
			},
			Agents:     sup,
			BuildAgent: buildAgentDef,
			Tenancy:    tenReg,
			Studio:     studioBroker,
			Workspace:  studioWS,
			Channels:   channelsStore,
			Safety:     safetyStore,
			StudioCatalog: func() studio.Catalog {
				models := make([]string, 0)
				for name := range llmMgr.List() {
					models = append(models, name)
				}
				safety := []string{"default", "none"}
				for name := range cfg.Profiles.Safety {
					safety = append(safety, name)
				}
				memory := make([]string, 0)
				for name := range cfg.Profiles.Memory {
					memory = append(memory, name)
				}
				prompts := make([]string, 0)
				for name := range cfg.Prompts {
					prompts = append(prompts, name)
				}
				return studio.NewCatalog(models, toolReg.Names(), safety, memory, prompts, config.DefaultCapabilities)
			},
			Users: webui.UserDeps{
				Identities: identReg,
				Store:      rtStore,
				Events:     eventLog,
				Journal:    journalSink,
				Quota:      userQuota,
				Files:      filesMgr,
				Purge:      rtStore,
			},
		})
		admin.Mount("/admin/api/", console.API(), true)
		// The docs are part of AgentFlow Studio now. Keep a one-release
		// signpost so old bookmarks and scripts get an explanation instead of
		// a bare 404; no assets are shipped in this binary.
		docsGone := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "documentation moved to AgentFlow Studio (agentflow-studio)", http.StatusGone)
		})
		admin.Mount("/docs", docsGone, false)
		admin.Mount("/docs/", docsGone, false)
		if consoleEnabled {
			admin.Mount("/", console.Static(), false)
			if tcpAdmin {
				log.Info("web console enabled", "url", "http://"+adminAddr+"/")
			} else {
				log.Info("web console HTTP disabled; the API is served on the local admin socket")
			}
		} else {
			log.Info("web console static files disabled; the admin and Studio APIs are still served")
		}
	}
	if tcpAdmin {
		go func() {
			if err := admin.Start(); err != nil {
				log.Warn("admin server stopped", "err", err)
			}
		}()
	}
	if adminSocket != "" {
		l, err := metrics.ListenLocalAdmin(adminSocket)
		if err != nil {
			log.Error("admin local listener failed", "socket", adminSocket, "err", err)
			os.Exit(1)
		}
		log.Info("admin local listener", "socket", adminSocket)
		go func() {
			if err := admin.Serve(l); err != nil {
				log.Warn("admin local listener stopped", "err", err)
			}
		}()
	}

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
	// One instance fires each occurrence; without this the fleet would deliver
	// every trigger once per instance.
	triggerSvc.SetLeases(leaseMgr)
	triggerSvc.Reload(cfg.Triggers)
	// A configdir's triggers/*.yaml is re-read on a poll, so editing a schedule
	// needs no restart (the rest of the directory still does).
	if cfgSource.IsDir {
		go triggerSvc.WatchConfigDir(ctx, cfgSource.Dir)
	}

	// Daemon agents (persistent: true) get a synthetic boot message so their
	// sessions spawn now — after channels are registered, so a boot-turn reply
	// has somewhere to go — instead of waiting for first external traffic.
	sup.BootPersistent(ctx)

	log.Info("agentflow up", "agents", len(defs), "channels", len(cfg.Gateway.Channels))
	<-ctx.Done()

	log.Info("shutting down")
	// Before the stores close: stopping the triggers gives up their claims, and
	// the hub gives up its sessions, so the rest of the fleet picks both up at
	// once instead of waiting out the leases.
	triggerSvc.Stop()
	hub.Release(ctx)
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

// warnInertConfig reports configuration that is parsed — so a config that sets
// it keeps booting — and that nothing reads. Each one claims a behaviour the
// engine does not have, so staying silent is how a deployment comes to believe
// it configured something.
//
// Every field here is a pointer for the same reason: presence has to be
// distinguishable from a zero value, and that is now their only purpose.
// Deleting one outright would turn an existing config into a parse error, since
// decoding is strict — a boot failure for a setting that does nothing.
func warnInertConfig(cfg *config.Config, log *slog.Logger) {
	// (runtime.vm.memory_limit used to warn here: it was inert — the Luau
	// state had no allocator cap. It is live now: main parses it and the
	// supervisor stamps every new session's state with the cap.)
	if v := cfg.Runtime.VM.InstructionBudget; v != nil {
		log.Warn("runtime.vm.instruction_budget has no effect: the per-resume budget is fixed at 5,000,000", "value", *v)
	}
	if v := cfg.Runtime.Reload.Watch; v != nil {
		log.Warn("runtime.reload.watch has no effect: the reload watcher always runs", "value", *v)
	}

	reportStore := func(profile string, stores map[string]config.Store) {
		for name, s := range stores {
			if s.Collection != nil {
				log.Warn("memory store collection has no effect: a store is bound by backend and table",
					"profile", profile, "store", name, "value", *s.Collection)
			}
			if s.Policy != nil {
				log.Warn("memory store policy has no effect: no policy knob is implemented for a store",
					"profile", profile, "store", name, "value", *s.Policy)
			}
		}
	}
	reportStore("built-in", config.DefaultMemoryProfile().Stores)
	for name, p := range cfg.Profiles.Memory {
		reportStore(name, p.Stores)
	}

	if v := cfg.Tools.Policy.Write; v != nil {
		log.Warn("tools.policy.write has no effect: no tool write policy is implemented", "value", *v)
	}
	// These three are accepted on an override and copied onto the tool, and
	// nothing ever consults them — so a deployment that sets one is configuring
	// a decision the engine does not make. (`permission` is the exception: it
	// works, as the exact string "forbidden".)
	for tname, o := range cfg.Tools.Policy.Overrides {
		if o.CostLevel != nil {
			log.Warn("tools.policy.overrides.cost_level has no effect: nothing reads a tool's cost level",
				"tool", tname, "value", *o.CostLevel)
		}
		if o.UserVisible != nil {
			log.Warn("tools.policy.overrides.user_visible has no effect: nothing reads a tool's visibility",
				"tool", tname, "value", *o.UserVisible)
		}
		if o.Autonomous != nil {
			log.Warn("tools.policy.overrides.autonomous has no effect: nothing reads a tool's autonomy",
				"tool", tname, "value", *o.Autonomous)
		}
	}
}

// gcNoOpProviders are the memory providers whose GC does not trim anything:
// each returns nil without deleting. A store that declares a window or a
// retention on one of them grows without bound while its config says otherwise
// — the same shape of untruth as a retention string that never parsed, which is
// why it is worth saying at boot.
//
// Named here rather than discovered, because the provider interface has no way
// to say "my GC is a no-op". Implementing the trimming means deleting the entry.
var gcNoOpProviders = map[string]bool{
	"pgvector":    true,
	"qdrant":      true,
	"redisvector": true,
	"redis":       true,
}

// warnUnenforceableGC reports stores whose backend cannot enforce the bound
// they declare. It is a warning and not a boot error on purpose: the setting is
// inert rather than harmful, and refusing to start would strand a deployment
// that has one — the same call the deprecated runtime: keys get.
func warnUnenforceableGC(cfg *config.Config, log *slog.Logger) {
	report := func(label string, stores map[string]config.Store) {
		for sname, s := range stores {
			if s.Window == 0 && s.Retention == "" {
				continue
			}
			b, ok := cfg.Memory.Backends[s.Backend]
			if !ok || !gcNoOpProviders[b.Provider] {
				continue
			}
			log.Warn("memory store declares a bound its backend does not enforce: this provider's GC deletes nothing, so the table grows without limit",
				"profile", label, "store", sname, "backend", s.Backend, "provider", b.Provider,
				"window", s.Window, "retention", s.Retention)
		}
	}
	report("built-in", config.DefaultMemoryProfile().Stores)
	for name, p := range cfg.Profiles.Memory {
		report(name, p.Stores)
	}
}

// loopbackListen reports whether an admin listen address is reachable only
// from this host, which is what makes a per-boot token a secret worth having.
//
// It is deliberately strict, and answers from the address text alone rather
// than resolving anything: an empty host (":9090") binds every interface, a
// bare port or an unparseable address is not vouched for, and a name other
// than "localhost" is not assumed to resolve to a loopback address. Anything
// this cannot vouch for is treated as reachable and requires a real token.
// hostIsLoopback reports whether a URL host is a loopback address. It is used
// only to keep operator-configured local MCP sidecars working under the
// outbound address guard; it is not an input from a model or a channel.
func hostIsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// resolveAdminToken returns the bearer token the admin plane will require.
//
// It mints a per-boot token whenever ADMIN_TOKEN is unset — not only when the
// console is on, which is what it used to do. Generating it inside
// `if !*noWebUI` meant `-no-webui` with no ADMIN_TOKEN left an empty token, and
// metrics.AdminServer.auth reads an empty token as "no auth required": /metrics,
// /admin/sessions and credential provisioning were then served to anything that
// could reach the port.
//
// A generated token is only a secret if nothing else can reach that port, so a
// non-loopback listen with no ADMIN_TOKEN is refused rather than quietly
// served. envToken is a parameter rather than read here so both branches are
// testable.
func resolveAdminToken(envToken, listen string, log *slog.Logger) (string, error) {
	return resolveAdminTokenWithOperators(envToken, listen, false, log)
}

// resolveAdminTokenWithOperators is the boot path: named operator tokens make
// a non-loopback admin listener authenticatable without a shared ADMIN_TOKEN.
// The break-glass token is still minted and logged once.
func resolveAdminTokenWithOperators(envToken, listen string, hasOperators bool, log *slog.Logger) (string, error) {
	if log == nil {
		log = slog.Default()
	}
	if envToken != "" {
		return envToken, nil
	}
	if !loopbackListen(listen) && !hasOperators {
		return "", fmt.Errorf("runtime.admin.listen %q is not loopback and ADMIN_TOKEN is unset; set ADMIN_TOKEN, name runtime.admin.operators, or bind runtime.admin.listen to a loopback address", listen)
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("admin token generation: %w", err)
	}
	token := hex.EncodeToString(b)
	log.Info("admin token (set ADMIN_TOKEN to pin it)", "token", token)
	log.Warn("admin token is per-boot and per-instance: a fleet must set ADMIN_TOKEN, or every instance rejects the others' tokens")
	return token, nil
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
// loopViewOf projects a profile into what a loop may see: a display name and
// the handles, never an email address or a credential.
func loopViewOf(p identity.Profile) caps.LoopProfile {
	v := caps.LoopProfile{UserID: p.UserID, DisplayName: p.DisplayName}
	v.Identities = make([]caps.LoopIdentity, 0, len(p.Identities))
	for _, id := range p.Identities {
		v.Identities = append(v.Identities, caps.LoopIdentity{
			Channel: id.Channel, Username: id.Username, Name: id.Name,
		})
	}
	return v
}

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

// resolveSafety turns a safety profile reference into a dispatcher. "" or
// "none" opts out entirely; "default" is the built-in baseline; any other name
// is a profiles.safety entry. That entry may mix baseline filters with
// classifier:<name> references, which resolve against the process's classifier
// registry.
//
// An unresolvable reference is an error, not a silent downgrade. A previous
// version fell through to safety.None, so a typo turned the chain off; the
// agent build (and Studio Apply) now fails closed instead.
func resolveSafety(cfg *config.Config, ref string, classifiers map[string]*moderation.Client, agent string) (*safety.Dispatcher, error) {
	switch ref {
	case "", "none":
		return safety.New(safety.None), nil
	case "default":
		return safety.New(safety.DefaultProfile()), nil
	}
	sp, ok := cfg.Profiles.Safety[ref]
	if !ok {
		return nil, fmt.Errorf("safety profile %q is not configured", ref)
	}
	filters := make([]safety.Filter, 0, len(sp.Filters))
	for _, name := range sp.Filters {
		if cname, ok := strings.CutPrefix(name, "classifier:"); ok {
			client := classifiers[cname]
			profile, exists := cfg.Safety.Classifiers[cname]
			if !exists || client == nil {
				return nil, fmt.Errorf("safety profile %q references classifier %q, which is not available in this process", ref, cname)
			}
			filters = append(filters, safety.NewClassifierFilter(classifierOptions(cname, profile, client, agent)))
			continue
		}
		f, ok := safety.Builtin(name)
		if !ok {
			return nil, fmt.Errorf("safety profile %q references unknown filter %q", ref, name)
		}
		filters = append(filters, f)
	}
	return safety.New(&safety.Profile{Name: ref, Filters: filters}), nil
}

// classifierOptions translates a config definition into the safety filter's
// policy. Defaults that are not applied during validation live here so a
// hand-built config (tests) behaves like a loaded one.
func classifierOptions(name string, p config.ClassifierProfile, client *moderation.Client, agent string) safety.ClassifierOptions {
	ingress, egress := true, true
	if len(p.Directions) > 0 {
		ingress, egress = false, false
		for _, d := range p.Directions {
			switch d {
			case "ingress":
				ingress = true
			case "egress":
				egress = true
			}
		}
	}
	maxInput := p.MaxInputChars
	if maxInput == 0 {
		maxInput = 8000
	}
	return safety.ClassifierOptions{
		Name:          name,
		Agent:         agent,
		Classifier:    client,
		Ingress:       ingress,
		Egress:        egress,
		Sources:       p.Sources,
		Threshold:     p.ThresholdValue(),
		Categories:    p.Categories,
		Mode:          p.Mode,
		Timeout:       p.TimeoutD(),
		OnError:       p.OnError,
		OnSaturated:   p.OnSaturated,
		OnBudget:      p.OnBudget,
		MaxInputChars: maxInput,
		Refusal:       p.Refusal,
		Fallback:      p.Fallback,
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

// parseByteSize reads a byte size as a config value: a plain byte count, or a
// number with a b/kb/mb/gb (case-insensitive, binary multiples) suffix — the
// spellings "64mb" and "1048576" both work. nil or "" means unset.
func parseByteSize(v *string) (int64, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return 0, nil
	}
	s := strings.TrimSpace(strings.ToLower(*v))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "gb"):
		mult, s = 1<<30, strings.TrimSuffix(s, "gb")
	case strings.HasSuffix(s, "mb"):
		mult, s = 1<<20, strings.TrimSuffix(s, "mb")
	case strings.HasSuffix(s, "kb"):
		mult, s = 1<<10, strings.TrimSuffix(s, "kb")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimSuffix(s, "b")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a byte size: %q", *v)
	}
	if n < 0 {
		return 0, fmt.Errorf("a byte size cannot be negative: %q", *v)
	}
	return n * mult, nil
}

func memoryFromConfig(s config.Store) memory.Store {
	ret := memory.Store{
		Backend:  s.Backend,
		Table:    s.Table,
		Window:   s.Window,
		Requires: s.Requires,
		Scope:    s.Scope,
		Pool:     s.Pool,
	}
	if s.Retention != "" {
		// Validated at config load, so this cannot fail here. It used to be
		// time.ParseDuration with the error discarded, and Go has no day unit —
		// so the shipped `retention: "30d"` silently became zero, which reads as
		// "never expires": the field looked configured and did nothing.
		d, _ := config.ParseRetention(s.Retention)
		ret.Retention = d
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

// resolveMediaS3 resolves the S3 credential pair for a blob-store backend
// (media or files — `what` labels the warning). Unresolvable logs a warning
// naming the credential and reports ok=false — the store is skipped.
func resolveMediaS3(ctx context.Context, res *config.Resolver, s3 config.MediaS3, what string, log *slog.Logger) (config.MediaS3, bool) {
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
			log.Warn(what+" store skipped: unresolved credential",
				"field", key.field, "credential", config.CredentialName(*key.dst))
			return config.MediaS3{}, false
		}
		*key.dst = v
	}
	return out, true
}

// splitUsage writes the ledger's two halves to the two places they belong: the
// rollup — which every quota check and budget refresh reads before a call — to
// the transactional store where its sums are exact and immediate, and the
// per-call detail to the log plane. It is what the metering path holds when a
// deployment configures one.
type splitUsage struct {
	rollups runtime.Ledger
	events  runtime.EventLog
}

func (w splitUsage) RecordUsage(rec runtime.UsageRecord, withEvent bool) error {
	if err := w.rollups.RecordUsage(rec, false); err != nil {
		return err
	}
	if !withEvent {
		return nil
	}
	// EventFrom applies the same normalisation the rollup just did, so the
	// detail agrees with the rollup it sums into.
	return w.events.AppendEvent(rec.UserID, runtime.EventFrom(rec))
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
