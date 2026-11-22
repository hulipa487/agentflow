// agentflow — phase 2 entrypoint: memory, tools, MCP, router, channels, llm.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"

	"agentflow/internal/adminapi"
	"agentflow/internal/builtins"

	"agentflow/internal/config"
	"agentflow/internal/core/accounting"
	"agentflow/internal/core/budget"
	"agentflow/internal/core/bundle"
	"agentflow/internal/core/caps"
	"agentflow/internal/core/chatreq"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/files"
	"agentflow/internal/core/fleet"
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
	"agentflow/internal/core/shelljobs"
	"agentflow/internal/core/shellops"
	"agentflow/internal/core/storedb"
	"agentflow/internal/core/studio"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/core/telemetry"
	"agentflow/internal/core/tenancy"
	"agentflow/internal/core/tools"
	"agentflow/internal/core/triggers"
	"agentflow/internal/core/users"
	"agentflow/internal/core/workspace"
	"agentflow/internal/drivers/bridge"
	"agentflow/internal/drivers/browser"
	"agentflow/internal/drivers/chatapi"
	"agentflow/internal/drivers/feishu"
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
	"agentflow/internal/drivers/qqbot"
	"agentflow/internal/drivers/redis"
	"agentflow/internal/drivers/redisvector"
	"agentflow/internal/drivers/s3media"
	"agentflow/internal/drivers/sandbox"
	"agentflow/internal/drivers/search"
	"agentflow/internal/drivers/shell"
	"agentflow/internal/drivers/sqlite"
	"agentflow/internal/drivers/telegram"
	"agentflow/internal/drivers/volatile"
	"agentflow/internal/drivers/webhook"
	"agentflow/internal/drivers/wecom"
	"agentflow/internal/llog"
	"agentflow/internal/luacode"
)

// version is stamped via -ldflags "-X main.version=..." on release builds.
var version = "dev"

func main() {
	// Every command but run is standalone and never boots the engine.
	args := hoistConfigFlags(os.Args[1:])
	if handled, code := runSubcommand(args, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	flags, code, ok := parseRunFlags(args, os.Stderr)
	if !ok {
		os.Exit(code)
	}
	cfgPath, forceSingle := &flags.config, &flags.forceSingle

	startedAt := time.Now()

	lvl, err := llog.ParseLevel(flags.logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		os.Exit(2)
	}
	var logOut io.Writer = os.Stderr
	if flags.logFile != "" {
		f, err := os.OpenFile(flags.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "agentflow: log file:", err)
			os.Exit(1)
		}
		defer f.Close()
		logOut = io.MultiWriter(os.Stderr, f)
	}
	format := strings.ToLower(strings.TrimSpace(flags.logFormat))
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

	cfgSource, err := config.ResolveSource(*cfgPath)
	if err != nil {
		log.Error("config source", "err", err)
		os.Exit(2)
	}
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

	// Register the runtime's core-contract embedded Lua chunks and wire
	// plugins.dir (which may shadow any registered builtin by name) plus
	// packages before the first luacode.Resolve below.
	builtins.Register()
	luacode.SetPluginDir(cfg.Plugins.Dir)
	luacode.SetPackages(cfg.Packages)

	// Every session loads the support chunks into its Lua state before its loop,
	// so they are policy exactly as a loop is, and a chunk this core cannot
	// serve is refused at boot for the same reason a loop is: the deployment
	// stops here, loudly, instead of coming up and failing session by session
	// against an API its Lua was not written for. See luacode.SupportChunks.
	if _, err := luacode.SupportChunks(); err != nil {
		log.Error("support chunk refused", "err", err)
		os.Exit(1)
	}

	// Two contexts, because shutdown has two moments. sigCtx ends when the
	// operator asks the process to stop; ctx is what the router, the session
	// actors, the hub and every periodic job run on, and it ends only after the
	// drain below. Running them on the signal context would stop all serving
	// at the signal, leaving the grace period nothing to drain.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	// Watch stdin so a ctrl+c byte typed into a pseudo-terminal (Git
	// Bash/mintty, some IDE terminals) still stops the engine on Windows. A
	// real console is unaffected: its ctrl+c arrives as SIGINT, not a byte.
	watchStdinInterrupt(stop)

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
	// cleared with a warning (the profile itself stays — a sandbox profile has
	// no password, and ssh may still authenticate by key).
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
	memReg.SetTopology(cfg.Topology())
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
	warnTopologyConfig(cfg, log)

	// Drivers and shared infrastructure.
	llmMgr := llm.NewManager(cfg.Models, log)
	// The OpenAI channel's parsed requests live here, in Go, not in Lua (D6).
	// The channel writes; llm.forward and openai.request read. One registry
	// per process is enough: refs are UUIDs, and every channel shares the same
	// op handler maps.
	openaiRequests := chatreq.NewRegistry()
	llmMgr.SetSecretResolver(func(raw string) (string, bool) {
		return credResolver.Resolve(ctx, raw)
	})
	// runtime.scheduler.workers is the one place the pool size is set.
	workerCount := 8
	if w := cfg.Runtime.Scheduler.Workers; w != nil {
		workerCount = *w
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

	// Remote shells: a connection to a real machine, held for a session. Shell
	// commands on this host are not handles at all — they run as one-off
	// sandbox jobs, wired below once the files store exists.
	shellMgr := shell.NewManager([]shell.ShellProvider{
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

	// Instance registry: one row per process using this store, refreshed as a
	// heartbeat. It is what makes a `single` deployment refuse a stray second
	// process instead of silently behaving as an unplanned fleet, and what lets
	// an operator see every instance behind one admin API.
	fleetReg, err := fleet.Open(fleet.Config{
		Target:      cfg.PersistencePath(),
		Owner:       leaseMgr.Owner(),
		Region:      cfg.Runtime.Region,
		Zone:        cfg.Runtime.Zone,
		Version:     version,
		Topology:    cfg.Topology(),
		ConfigEpoch: cfg.Epoch,
		Log:         log,
	})
	if err != nil {
		log.Error("instance registry failed", "err", err)
		os.Exit(1)
	}
	defer fleetReg.Close()
	if !cfg.IsRegional() {
		if err := fleetReg.CheckSingle(ctx); err != nil {
			if *forceSingle {
				log.Warn("-force-single: starting despite another live instance", "err", err)
			} else {
				log.Error("single topology refuses a second instance", "err", err)
				os.Exit(1)
			}
		}
	}
	fleetReg.Start(ctx)

	// The lease store differing from the runtime store is still possible in
	// `single` (runtime.cluster.persistence is deprecated, not refused), and it
	// is the smell that makes singleton work run once per instance. In a
	// regional cell validation refuses it unless it names the runtime store, so
	// only the single branch remains.
	clusterTarget := cfg.ClusterStore()
	if !cfg.IsRegional() && storedb.BackendFor(clusterTarget) != storedb.BackendFor(cfg.PersistencePath()) {
		log.Warn("the lease store is on a different backend from the runtime store: singleton work will run once per instance rather than once per deployment",
			"lease_store", storedb.Display(clusterTarget))
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

	// singletonFor runs fn only while this instance holds the named lease for
	// ttl. The TTL belongs to the job: a job that repeats should hold its lease
	// for at least its interval plus slack, or the lease lapses between two of
	// its own ticks and a peer runs the job as well (see housekeepingTTL).
	singletonFor := func(ctx context.Context, name string, ttl time.Duration, fn func()) {
		claimAndRun(ctx, leaseMgr, name, ttl, log, fn)
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
		identReg.SetAutoClaim(!cfg.Runtime.Identity.RegistrationRequired())

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
	//
	// A regional cell keeps the in-flight holds in the shared store: two
	// instances admitting a call each against the same remaining budget is the
	// overspend the hold exists to prevent, and per-process maps cannot see
	// each other. Single keeps the in-memory map it always had.
	var userQuota *accounting.Quota
	if identReg != nil {
		if cfg.IsRegional() {
			userQuota, err = accounting.NewShared(cfg.PersistencePath(), rtStore, identReg.LimitFor, cfg.Usage.DefaultTokensPerDay)
			if err != nil {
				log.Error("shared quota holds failed", "err", err)
				os.Exit(1)
			}
			defer func() { _ = userQuota.Close() }()
		} else {
			userQuota = accounting.New(rtStore, identReg.LimitFor, cfg.Usage.DefaultTokensPerDay)
		}
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

	// Centralized media + files blob plane (Option 1B).
	//
	// Media owns one primary blob store (fs under <data_dir>/media or s3).
	// When files.* does not configure an explicit separate backend/dir/s3,
	// filesMgr shares that same blob store and one unified mark-sweep GC runs
	// over the union of files_meta handles and media grants. When files.*
	// configures its own separate store, it stays independent and mediaSvc
	// registers it as a read fallback.
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

	// Optional HMAC-SHA256 temporary HTTP URL publisher for serving blobs
	// (including local fs blobs) to URL-only channels such as QQ.
	var tempURLHandler *media.HTTPPresigner
	if cfg.Media.TempURL.Enabled && mediaStore != nil {
		secret := cfg.Media.TempURL.Secret
		secretOK := true
		if secret != "" {
			v, ok := credResolver.Resolve(ctx, secret)
			if !ok {
				log.Warn("media temp_url skipped: unresolved credential",
					"field", "media.temp_url.secret", "credential", config.CredentialName(secret))
				secretOK = false
			} else {
				secret = v
			}
		} else if cfg.Topology() != "single" {
			log.Warn("media.temp_url.secret is unset in a multi-instance topology; ephemeral per-boot signing keys cannot be verified across peers")
		}
		if secretOK {
			hp, herr := media.NewHTTPPresigner(mediaStore, cfg.Gateway.PublicURL, cfg.Media.TempURL.MediaTempURLPath(), secret)
			if herr != nil {
				log.Error("media temp_url setup failed", "err", herr)
				os.Exit(1)
			}
			tempURLHandler = hp
		}
	}

	// User-scoped file store: shares mediaStore by default (Option 1B), or
	// opens its own separate fs/S3 store when files.* configures one explicitly.
	var filesMgr *files.Manager
	var separateFilesStore media.Store
	sharedBlobStore := false
	if !cfg.Files.HasExplicitStore() && mediaStore != nil {
		filesMgr = files.New(mediaStore, rtStore, cfg.Files.FilesScratchTTL(), cfg.Files.FilesMaxBytes(), log)
		sharedBlobStore = true
	} else if cfg.Files.Backend == "s3" {
		s3cfg, ok := resolveMediaS3(ctx, credResolver, cfg.Files.S3, "files", log)
		if ok {
			blobStore, serr := s3media.New(s3cfg)
			if serr != nil {
				log.Error("files store failed", "err", serr)
				os.Exit(1)
			}
			separateFilesStore = blobStore
			filesMgr = files.New(blobStore, rtStore, cfg.Files.FilesScratchTTL(), cfg.Files.FilesMaxBytes(), log)
		}
	} else {
		filesDir := cfg.Files.Dir
		if filesDir == "" {
			filesDir = filepath.Join(cfg.DataDir(), "files")
		}
		blobStore, ferr := media.Open(filesDir)
		if ferr != nil {
			log.Error("files store failed", "err", ferr)
			os.Exit(1)
		}
		separateFilesStore = blobStore
		filesMgr = files.New(blobStore, rtStore, cfg.Files.FilesScratchTTL(), cfg.Files.FilesMaxBytes(), log)
	}

	// Centralized media service: handle ingestion, per-scope grant checks,
	// image transcoding + derived-variant caching, and presigned URL delivery.
	var mediaPresigner media.Presigner
	if tempURLHandler != nil {
		mediaPresigner = tempURLHandler
	} else if p, ok := mediaStore.(media.Presigner); ok {
		mediaPresigner = p
	}
	mediaSvc := media.NewService(media.ServiceOptions{
		Primary:      mediaStore,
		Secondary:    separateFilesStore,
		Presigner:    mediaPresigner,
		Catalog:      media.NewCatalog(runtime.MediaKV(rtStore)),
		Retention:    cfg.Media.MediaRetention(),
		DerivedTTL:   cfg.Media.MediaDerivedTTL(),
		PresignTTL:   cfg.Media.TempURL.MediaTempURLTTL(),
		HTTPClient:   &http.Client{Transport: netPolicy.Transport(), Timeout: 60 * time.Second},
		EnforceScope: true,
	})

	if filesMgr != nil {
		// Scratch expiry ticks hourly under a cluster lease.
		sweepScratch := func() {
			if n, err := filesMgr.SweepScratch(ctx); err != nil {
				log.Warn("files scratch sweep failed", "err", err)
			} else if n > 0 {
				metrics.Add("agentflow_files_scratch_swept", int64(n))
				log.Info("files scratch swept", "expired", n)
			}
		}
		singletonFor(ctx, "files-scratch", housekeepingTTL(time.Hour), sweepScratch)
		every(ctx, time.Hour, func() {
			singletonFor(ctx, "files-scratch", housekeepingTTL(time.Hour), sweepScratch)
		})
	}
	if sharedBlobStore && mediaSvc != nil && filesMgr != nil {
		// Unified mark-and-sweep over the shared blob store: preserve the
		// union of files_meta handles and unexpired media grants/variants.
		grace := cfg.Media.MediaGCGrace()
		if cfg.Media.GCGrace == "" && cfg.Files.GCGrace != "" {
			grace = cfg.Files.FilesGCGrace()
		}
		if grace > 0 {
			sweepShared := func() {
				filesLive, err := filesMgr.LiveHandles(ctx)
				if err != nil {
					log.Warn("unified blob gc skipped: failed to list live file handles", "err", err)
					return
				}
				if live, swept, err := mediaSvc.GC(ctx, grace, filesLive); err != nil {
					log.Warn("unified blob gc failed", "err", err)
				} else if swept > 0 {
					metrics.Add("agentflow_files_gc_swept", int64(swept))
					log.Info("unified blob gc swept", "live", live, "swept", swept)
				}
			}
			singletonFor(ctx, "media-gc", housekeepingTTL(time.Hour), sweepShared)
			every(ctx, time.Hour, func() {
				singletonFor(ctx, "media-gc", housekeepingTTL(time.Hour), sweepShared)
			})
		}
	} else {
		if filesMgr != nil {
			if grace := cfg.Files.FilesGCGrace(); grace > 0 {
				sweepBlobs := func() {
					if live, swept, err := filesMgr.GC(ctx, grace); err != nil {
						log.Warn("files blob gc failed", "err", err)
					} else if swept > 0 {
						metrics.Add("agentflow_files_gc_swept", int64(swept))
						log.Info("files blob gc swept", "live", live, "swept", swept)
					}
				}
				singletonFor(ctx, "files-gc", housekeepingTTL(time.Hour), sweepBlobs)
				every(ctx, time.Hour, func() {
					singletonFor(ctx, "files-gc", housekeepingTTL(time.Hour), sweepBlobs)
				})
			}
		}
		if mediaStore != nil && mediaSvc != nil {
			if grace := cfg.Media.MediaGCGrace(); grace > 0 {
				sweepMedia := func() {
					if live, swept, err := mediaSvc.GC(ctx, grace); err != nil {
						log.Warn("media blob gc failed", "err", err)
					} else if swept > 0 {
						log.Info("media blob gc swept", "live", live, "swept", swept)
					}
				}
				singletonFor(ctx, "media-gc", housekeepingTTL(time.Hour), sweepMedia)
				every(ctx, time.Hour, func() {
					singletonFor(ctx, "media-gc", housekeepingTTL(time.Hour), sweepMedia)
				})
			}
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
	// Sandbox jobs: one command, one ephemeral sandbox (gVisor runsc or
	// microsandbox msb), on a project from the files store. Nothing is probed
	// or started here — a deployment with no sandbox shell profile never invokes
	// the runtime — and an instance where the sandbox cannot work still boots:
	// the first shell call says why.
	envSpecs := make(map[string]sandbox.EnvSpec, len(cfg.Environments))
	for name, e := range cfg.Environments {
		envSpecs[name] = sandbox.EnvSpec{Image: e.Image, Rootfs: e.Rootfs, Env: e.Env}
	}
	sbx := cfg.Runtime.Sandbox
	sandboxDir := cfg.SandboxDataDir()
	sandboxRunner, sandboxInspector := newSandboxRunner(sbx, sandboxDir, log)
	sandboxEnvs := sandbox.NewEnvs(sandboxDir, envSpecs, log)
	if sbx.ProviderName() == "microsandbox" {
		sandboxEnvs.SetNativeImages(true)
	}
	shellJobs := shelljobs.New(shelljobs.Config{
		Concurrency:          sbx.Concurrency,
		PerScope:             sbx.PerUser,
		QueueTimeout:         sbx.QueueWait(),
		Timeout:              sbx.JobTimeout(),
		MaxTimeout:           sbx.JobMaxTimeout(),
		BackgroundMaxTimeout: sbx.BackgroundTimeout(),
		Limits: sandbox.Limits{
			OutputBytes: sbx.OutputBytes(),
			TmpBytes:    sbx.TmpBytes(),
			MemoryBytes: sbx.MemoryBytes(),
			CPUs:        sbx.CPUs,
			PIDs:        sbx.PIDs,
		},
		WriteBack: workspace.Limits{MaxFiles: sbx.WriteBackFiles(), MaxBytes: sbx.WriteBackBytes()},
	}, sandboxRunner, sandboxEnvs,
		workspace.New(filepath.Join(sandboxDir, "workspaces"), filesMgr, log), filesMgr, log)
	// Background jobs are recorded in the runtime store, so a session that
	// moved to another instance can still ask about the one it started.
	shellJobs.SetStore(ctx, rtStore, leaseMgr.Owner())
	shellFront := &shellops.Front{SSH: shellMgr, Jobs: shellJobs, Files: filesMgr}
	// A finished background job reports to its session's inbox. The supervisor
	// that delivers it is built further down, so the handlers get a slot that
	// is filled in then.
	var jobDelivery atomic.Pointer[caps.JobDeliverer]
	deliverJob := func(owner string, msg session.Message) {
		if d := jobDelivery.Load(); d != nil {
			(*d)(owner, msg)
		}
	}
	if sandboxInUse(cfg) {
		if err := shellJobs.Available(); err != nil {
			log.Warn("an agent has a sandbox shell profile, but shell jobs cannot run on this instance", "err", err)
		} else if enforced, note := sandboxInspector.Limiting(); !enforced {
			log.Warn("shell jobs will run without memory and CPU limits", "why", strings.TrimPrefix(note, "memory and CPU limits are not enforced: "))
		}
	}
	tools.RegisterShellBuiltins(toolReg, shellFront)
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
		c.SetMedia(mediaSvc)
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
		src, watchPath, err := luacode.Resolve(a.Loop)
		if err != nil {
			if h := loopHint(a.Loop, toolReg.Names(), luacode.Names()); h != "" {
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
			if h := skillHint(s, luacode.Names()); h != "" {
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
			Pool:     pool,
			Quota:    userQuota,
			Agent:    name,
			Ledger:   usageWriter,
			Events:   usageEvents,
			Log:      log,
			Requests: openaiRequests,
			Media:    mediaSvc,
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
		for k, h := range gateOps(enforce, name, effectiveCaps, "shell.exec", caps.ShellHandlers(shellFront, name, deliverJob), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "net.http", caps.HTTPHandlers(log, credStore, netPolicy, filesMgr), &withheld) {
			handlers[k] = limitEgress(h)
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "net.mail", caps.MailHandlers(log, credStore), &withheld) {
			handlers[k] = limitEgress(h)
		}
		for k, h := range gateOps(enforce, name, effectiveCaps, "files", caps.FileHandlersWithMedia(filesMgr, name, mediaSvc), &withheld) {
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
		src, watchPath, err := luacode.Resolve(p.Loop)
		if err != nil {
			fields := []any{"profile", pname, "err", err}
			if h := loopHint(p.Loop, toolReg.Names(), luacode.Names()); h != "" {
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
			if h := skillHint(s, luacode.Names()); h != "" {
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
			Pool:     pool,
			Quota:    userQuota,
			Agent:    pname,
			Ledger:   usageWriter,
			Events:   usageEvents,
			Log:      log,
			Requests: openaiRequests,
			Media:    mediaSvc,
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
		for k, h := range gateOps(enforce, pname, profileCaps, "shell.exec", caps.ShellHandlers(shellFront, pname, deliverJob), &withheld) {
			handlers[k] = h
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "net.http", caps.HTTPHandlers(log, credStore, netPolicy, filesMgr), &withheld) {
			handlers[k] = limitEgress(h)
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "net.mail", caps.MailHandlers(log, credStore), &withheld) {
			handlers[k] = limitEgress(h)
		}
		for k, h := range gateOps(enforce, pname, profileCaps, "files", caps.FileHandlersWithMedia(filesMgr, pname, mediaSvc), &withheld) {
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
		historyBudget := p.HistoryBudget
		if historyBudget <= 0 {
			historyBudget = 6000
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
				HistoryBudget: historyBudget,
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

	exit := os.Exit
	sup := supervisor.New(defs, gw, opPool, shellMgr, log)
	sup.SetShellJobs(shellJobs)
	sup.SetMedia(mediaSvc)
	deliverJobResult := caps.JobDeliverer(func(owner string, msg session.Message) {
		agent, key, ok := sessionhub.SplitSessionKey(owner)
		if !ok {
			log.Warn("shell job result has a malformed owner; dropping it", "owner", owner)
			return
		}
		// Routed like any message: with a hub it goes to whichever instance
		// owns the session now, which need not be the one that ran the job.
		if err := sup.Deliver(agent, key, msg); err != nil {
			log.Warn("shell job result could not be delivered", "session", owner, "job", msg.ID, "err", err)
		}
	})
	jobDelivery.Store(&deliverJobResult)
	sup.SetPushSameTenant(cfg.Runtime.Users.PushSameTenant)

	// Studio (the standalone builder/playground app): one broker both observes
	// every session (trace rings) and acts as the synthetic "studio" channel,
	// so a playground reply arrives through the ordinary egress path — safety,
	// journaling and delivery unchanged.
	var studioBroker *studio.Broker
	var studioWS *studio.Workspace
	var channelsStore *studio.Channels
	var safetyStore *studio.Safety
	var memoryStore *studio.Memory
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
		memoryStore = studio.NewMemory(cfg, cfgSource.ConfigDir(), cfgSource.ConfigPath(), log)
	}

	var telemetryExporter *telemetry.Exporter
	if cfg.Observability.Tracing.Enabled {
		telemetryExporter = startTelemetryExporter(ctx, cfg.Observability.Tracing, credResolver, log)
	}
	var tracers []session.Tracer
	if studioBroker != nil {
		tracers = append(tracers, studioBroker.Trace)
	}
	if telemetryExporter != nil {
		tracers = append(tracers, telemetryExporter.Trace)
	}
	if combined := session.MultiTracer(tracers...); combined != nil {
		tracePayloads := (studioBroker != nil && cfg.Runtime.Studio.TracePayloads) ||
			(telemetryExporter != nil && cfg.Observability.Tracing.IncludePayloads)
		sup.SetTracer(combined, tracePayloads)
	}
	// Per-tenant resource bounds (E18). Both are opt-in: 0 disables. The op
	// pool's fairness, the sharpest bound, is not here — it is how the pool
	// works now, not a setting.
	sup.SetSessionCap(cfg.Runtime.Limits.SessionsPerTenant)
	// Ephemeral sessions have their own bound: a burst of stateless requests
	// must not consume the tenant's conversation slots.
	sup.SetEphemeralCap(cfg.Runtime.EphemeralCap())
	// Idle conversations are unloaded after runtime.sessions.idle_unload
	// (default 30m, 0 disables). The loop starts from the top on the next
	// message; durable state belongs in session.state.*.
	sup.SetIdleUnload(cfg.Runtime.IdleUnload())
	if cfg.Runtime.VM.MemoryLimit != nil && strings.TrimSpace(*cfg.Runtime.VM.MemoryLimit) != "" {
		if mem, err := config.ParseByteSize(*cfg.Runtime.VM.MemoryLimit); err != nil {
			log.Error("runtime.vm.memory_limit is not a size", "value", *cfg.Runtime.VM.MemoryLimit, "err", err)
			exit(1)
		} else if mem > 0 {
			sup.SetVMMemory(mem)
		}
	}

	// Session hub. A conversation runs on one instance at a time, and in a
	// fleet any instance may receive its traffic — a webhook load-balanced
	// across the fleet, a trigger firing wherever its lease landed, a push from
	// another node. The hub puts a per-session lease in front of delivery and
	// the shared inbox behind it: an instance that owns the session (or can
	// take it) delivers the message itself, and one that does not queues it for
	// the owner. Without a shared store both are local, the lease is
	// uncontended, and this behaves exactly as it did before.
	// A single process that is the only process needs no per-session lease: the
	// lease exists to decide which instance runs a conversation, and there is
	// only one. -force-single keeps it, because the flag's whole point is that
	// two processes may share the store after all — and then the lease is the
	// only thing keeping a session from running twice.
	useHub := useSessionHub(cfg, *forceSingle)
	var hub *sessionhub.Hub
	var hubQueue *inbox.Queue
	if useHub {
		hubQueue, err = inbox.Open(cfg.ClusterStore(), leaseMgr.Owner(), log)
		if err != nil {
			log.Error("session inbox failed", "err", err)
			exit(1)
		}
		defer hubQueue.Close()
		hub = sessionhub.New(leaseMgr, hubQueue, sup.DeliverLocal, log)
		// A store in another region makes the hub's two periods a deployment's
		// business: the poll is the added latency a message pays when it arrives
		// on the wrong instance, and the TTL is how long a killed instance's
		// sessions stay unavailable. Unset, the hub's own defaults (250ms, 30s)
		// stand.
		if d := cfg.Runtime.Cluster.ClusterPollInterval(); d > 0 {
			hub.SetPoll(d)
		}
		if d := cfg.Runtime.Cluster.ClusterSessionTTL(); d > 0 {
			hub.SetSessionTTL(d)
		}
		sup.SetHub(hub)
		// A session this instance loses to a peer must stop locally at once, not
		// keep running turns the new owner is also running. StopSessionForLease
		// cancels the actor without retiring the id, so this instance can run the
		// conversation again if it later wins the lease back.
		hub.SetOnLost(func(sessKey string) {
			log.Warn("session lease lost to another instance; stopping the local actor", "session", sessKey)
			sup.StopSessionForLease(sessKey)
		})
		// Instance-local memory is keyed by the ownership generation: when a
		// session moves to another instance and comes back, the rows its earlier
		// life wrote here are invisible instead of being mistaken for the current
		// conversation. A single process has one generation (0), so the source
		// stays unset.
		memMgr.SetGenerationSource(hub.Generation)
		go hub.Drain(ctx)
	} else {
		log.Info("single topology: session leases and the shared inbox are off")
	}
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

	// Daemon failover. BootPersistent runs once per instance at startup, so a
	// daemon whose instance is killed stays down until some instance restarts.
	// Check every half session TTL: FailoverPersistent asks the lease table
	// whether anyone owns the daemon and claims it when nobody does, so a peer
	// that also notices still runs the boot turn once.
	if hub != nil {
		failoverEvery := hub.SessionTTL() / 2
		if failoverEvery < time.Second {
			failoverEvery = time.Second
		}
		every(ctx, failoverEvery, func() { sup.FailoverPersistent(ctx) })
	}

	// Expired key/value rows — a route handler's state, the file store's
	// scratch — are reclaimed once a day. A row's deadline is a promise the
	// store keeps on read; this is what keeps the space, including for rows
	// nothing reads again. The same pass reclaims delivered inbox rows (which
	// otherwise grow without bound) and instance rows whose process is long
	// gone.
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
		if hubQueue != nil {
			if n, err := hubQueue.Sweep(ctx); err != nil {
				log.Warn("inbox sweep failed", "err", err)
			} else if n > 0 {
				log.Info("delivered inbox rows reclaimed", "rows", n)
			}
		}
		if n, err := fleetReg.Sweep(ctx, time.Now().Add(-fleet.SweepAfter)); err != nil {
			log.Warn("instance registry sweep failed", "err", err)
		} else if n > 0 {
			log.Info("stale instance rows reclaimed", "rows", n)
		}
		if userQuota != nil {
			if n, err := userQuota.SweepHolds(ctx, time.Now()); err != nil {
				log.Warn("quota hold sweep failed", "err", err)
			} else if n > 0 {
				log.Info("expired quota holds reclaimed", "rows", n)
			}
		}
	}
	sweepRowsTTL := housekeepingTTL(24 * time.Hour)
	singletonFor(ctx, "rows-sweep", sweepRowsTTL, sweepRows)
	every(ctx, 24*time.Hour, func() {
		singletonFor(ctx, "rows-sweep", sweepRowsTTL, sweepRows)
	})

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
		pruneTTL := housekeepingTTL(24 * time.Hour)
		singletonFor(ctx, "usage-prune", pruneTTL, prune)
		every(ctx, 24*time.Hour, func() {
			singletonFor(ctx, "usage-prune", pruneTTL, prune)
		})
	}

	// Message journal (core-owned audit). Every inbound event is recorded at
	// the router and every egress at the session actor; loops and channels
	// can neither skip nor forge it. Journal errors are logged, never fatal.
	//
	// The mode is per channel and both call sites apply it: the supervisor's
	// lookup serves every actor's egress, and the inbound callback below reads
	// the same map. A channel with type: openai defaults to metadata, so its
	// coding context is never written to the audit trail.
	journalModes := make(map[string]session.JournalMode, len(cfg.Gateway.Channels))
	for i, ch := range cfg.Gateway.Channels {
		name := ch.Name
		if name == "" {
			name = fmt.Sprintf("%s-%d", ch.Type, i)
		}
		journalModes[name] = session.ParseJournalMode(ch.JournalMode())
	}
	sup.SetJournalMode(func(channel string) session.JournalMode {
		if m, ok := journalModes[channel]; ok {
			return m
		}
		return session.JournalFull
	})
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
			pruneTTL := housekeepingTTL(24 * time.Hour)
			singletonFor(ctx, "journal-prune", pruneTTL, prune)
			every(ctx, 24*time.Hour, func() {
				singletonFor(ctx, "journal-prune", pruneTTL, prune)
			})
		}
		sup.EgressJournal = func(rec session.EgressRecord) {
			errStr := rec.Err
			entry := runtime.JournalEntry{
				ID:              rec.InReplyTo,
				Direction:       "out",
				Status:          rec.Status,
				Channel:         rec.Channel,
				Chat:            rec.ReplyTo,
				Sender:          rec.ReplyTo,
				UserUUID:        rec.UserUUID,
				Agent:           rec.Agent,
				SessionID:       rec.SessionID,
				Type:            "agent",
				Text:            rec.Text,
				Attachments:     rec.Attachments,
				TextBytes:       rec.TextBytes,
				AttachmentBytes: rec.AttachmentBytes,
				Err:             errStr,
			}
			if entry.ID == "" {
				entry.ID = uuid.NewString()
			}
			if err := journalSink.RecordMessage(ctx, entry); err != nil {
				log.Warn("journal egress write failed", "err", err)
			}
		}
	}

	// Admin API log tail: mirror every record into a ring buffer so Studio's
	// SSE log endpoint can stream it.
	logRing := adminapi.NewLogRing(500)
	log = slog.New(adminapi.NewTeeHandler(log.Handler(), logRing))
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
	routeSrc, routeWatchPath, err := luacode.Resolve(routeRef)
	if err != nil {
		log.Error("route plugin resolve failed", "err", err)
		exit(1)
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
			mode := journalModes[in.Channel]
			if mode == session.JournalOff {
				return
			}
			entry := runtime.JournalEntry{
				ID:              msg.ID,
				Ts:              ts,
				Direction:       "in",
				Status:          status,
				Channel:         in.Channel,
				Chat:            chat,
				Sender:          msg.From,
				UserUUID:        userUUID,
				Agent:           in.Agent,
				Type:            msg.Type,
				Text:            msg.Text,
				Attachments:     msg.Attachments,
				TextBytes:       len(msg.Text),
				AttachmentBytes: session.AttachmentBytes(msg.Attachments),
				Provenance:      prov,
			}
			// metadata keeps the row and its sizes and drops the payload; off
			// keeps nothing. For a channel that processes private code, a row
			// nobody can read is worse than no row.
			if mode == session.JournalMetadata {
				entry.Text = ""
				entry.Attachments = nil
			}
			if err := journalSink.RecordMessage(ctx, entry); err != nil {
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
		sc.SetRejectUnregistered(cfg.Runtime.Identity.RejectUnregistered())
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
			"require_registration", cfg.Runtime.Identity.RegistrationRequired())
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
	if tempURLHandler != nil {
		httpSrv.Handle(tempURLHandler.Path(), tempURLHandler.ServeHTTP)
		log.Info("media temporary URL endpoint enabled",
			"path", tempURLHandler.Path(),
			"ttl", cfg.Media.TempURL.MediaTempURLTTL().String())
	}
	// The OpenAI channel refuses requests while the instance drains. The admin
	// server is constructed later in this function (it needs the stores), after
	// the listener is already serving, so the check is published atomically:
	// a request can arrive between the two.
	var readyFn atomic.Pointer[func() bool]

	// User-facing profile API (opt-in): registration and channel linking on
	// the same public listener the HTTP channels attach to. Off by default —
	// the surface is public, so opening it is a deliberate act.
	if cfg.Runtime.Users.Enabled {
		if identReg == nil {
			// Config validation already rejects this pairing; the check stays
			// so a future refactor cannot mount a dead endpoint.
			log.Error("users API requires runtime.identity.enabled")
			exit(1)
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
				exit(1)
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
			Region:           cfg.Runtime.Region,
		}).Handler().ServeHTTP)
		log.Info("user API enabled", "prefix", users.Prefix,
			"registration", cfg.Runtime.Users.RegistrationMode(),
			"linkable", identReg.LinkableChannels(),
			"cors_origins", len(cfg.Runtime.Users.CORSOrigins))
	}
	var telegramDrivers []*telegram.Driver
	var qqDrivers []*qqbot.Driver
	var feishuDrivers []*feishu.Driver
	var wecomDrivers []*wecom.Driver
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
		resolveField := func(field, raw string, required bool) (string, bool) {
			if raw == "" {
				return "", !required
			}
			v, ok := credResolver.Resolve(ctx, raw)
			if !ok {
				log.Warn("channel skipped: unresolved credential",
					"channel", name, "field", field, "credential", config.CredentialName(raw))
				return "", false
			}
			return v, true
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
			d.SetMedia(mediaSvc)
			gw.Register(d)
			telegramDrivers = append(telegramDrivers, d)
		case "qq":
			if mediaSvc == nil || !mediaSvc.CanPresign() {
				log.Warn("channel skipped: qq requires public media URLs — configure media.backend: s3 or media.temp_url.enabled: true (with gateway.public_url)",
					"channel", name)
				continue
			}
			secret, ok := credResolver.Resolve(ctx, ch.Secret)
			if !ok {
				log.Warn("channel skipped: unresolved credential",
					"channel", name, "field", "secret", "credential", config.CredentialName(ch.Secret))
				continue
			}
			d := qqbot.New(name, ch.Agent, qqbot.Options{
				AppID:      ch.AppID,
				Secret:     secret,
				Mode:       ch.Mode,
				Path:       ch.QQPath(),
				PublicURL:  cfg.Gateway.PublicURL,
				Scenes:     ch.Scenes,
				APIBase:    ch.APIBase,
				Markdown:   ch.Markdown,
				Media:      mediaSvc,
				PresignTTL: cfg.Media.TempURL.MediaTempURLTTL(),
			}, sink, httpSrv, mediaStore, mpol, log)
			gw.Register(d)
			qqDrivers = append(qqDrivers, d)
		case "feishu":
			secret, ok := resolveField("secret", ch.Secret, true)
			if !ok {
				continue
			}
			token, ok := resolveField("token", ch.Token, false)
			if !ok {
				continue
			}
			encryptKey, ok := resolveField("encrypt_key", ch.EncryptKey, false)
			if !ok {
				continue
			}
			d := feishu.New(name, ch.Agent, feishu.Options{
				AppID:      ch.AppID,
				Secret:     secret,
				Token:      token,
				EncryptKey: encryptKey,
				Mode:       ch.Mode,
				Path:       ch.FeishuPath(),
				PublicURL:  cfg.Gateway.PublicURL,
				Scenes:     ch.Scenes,
				APIBase:    ch.APIBase,
				Markdown:   ch.Markdown,
				Media:      mediaSvc,
			}, sink, httpSrv, mediaStore, mpol, log)
			gw.Register(d)
			feishuDrivers = append(feishuDrivers, d)
		case "wecom":
			secret, ok := resolveField("secret", ch.Secret, true)
			if !ok {
				continue
			}
			token, ok := resolveField("token", ch.Token, true)
			if !ok {
				continue
			}
			encryptKey, ok := resolveField("encrypt_key", ch.EncryptKey, true)
			if !ok {
				continue
			}
			d := wecom.New(name, ch.Agent, wecom.Options{
				CorpID:     ch.CorpID,
				AgentID:    ch.AgentID,
				Secret:     secret,
				Token:      token,
				EncryptKey: encryptKey,
				Path:       ch.WeComPath(),
				PublicURL:  cfg.Gateway.PublicURL,
				APIBase:    ch.APIBase,
				Markdown:   ch.Markdown,
				Media:      mediaSvc,
			}, sink, httpSrv, mediaStore, mpol, log)
			gw.Register(d)
			wecomDrivers = append(wecomDrivers, d)
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
			d.SetMedia(mediaSvc)
			gw.Register(d)
		case "openai":
			agents := ch.Agents
			if len(agents) == 0 && ch.Agent != "" {
				agents = []string{ch.Agent}
			}
			keys := make([]string, 0, len(ch.Keys))
			unresolved := false
			for _, raw := range ch.Keys {
				v, ok := credResolver.Resolve(ctx, raw)
				if !ok {
					log.Warn("channel skipped: unresolved credential",
						"channel", name, "field", "keys", "credential", config.CredentialName(raw))
					unresolved = true
					break
				}
				keys = append(keys, v)
			}
			if unresolved {
				continue
			}
			var timeout time.Duration
			if ch.Timeout != "" {
				timeout, _ = time.ParseDuration(ch.Timeout) // validated at config load
			}
			opts := chatapi.Options{
				Path:            ch.Path,
				Agents:          agents,
				Auth:            ch.Auth,
				Keys:            keys,
				Timeout:         timeout,
				MaxRequestBytes: ch.MaxRequestBytes,
				ExposeReasoning: ch.ExposeReasoning,
				Ready: func() bool {
					if fn := readyFn.Load(); fn != nil {
						return (*fn)()
					}
					return true // still booting: the admin plane is not up yet
				},
				StopSession: sup.StopSession,
				Store:       mstore,
				Policy:      mpol,
				Requests:    openaiRequests,
			}
			if ch.Auth == "user_token" {
				if identReg == nil {
					log.Warn("channel skipped: auth user_token requires runtime.identity.enabled", "channel", name)
					continue
				}
				opts.TokenUser = identReg.TokenUser
			}
			d := chatapi.New(name, opts, sink, httpSrv, log)
			gw.Register(d)
		}
	}
	// Bind the shared listener now that every path is mounted, BEFORE the
	// telegram auto probe runs (a probe before bind would fail and force
	// polling). A bind failure (port in use, bad addr) is fatal and must
	// surface before "agentflow up".
	if err := httpSrv.Start(); err != nil {
		log.Error("httpd server failed to start", "listen", cfg.Gateway.Listen, "err", err)
		exit(1)
	}
	for _, d := range telegramDrivers {
		if err := d.Start(ctx); err != nil {
			log.Error("channel start failed", "channel", d.Name(), "err", err)
			exit(1)
		}
	}
	for _, d := range qqDrivers {
		if err := d.Start(ctx); err != nil {
			log.Error("channel start failed", "channel", d.Name(), "err", err)
			exit(1)
		}
	}
	for _, d := range feishuDrivers {
		if err := d.Start(ctx); err != nil {
			log.Error("channel start failed", "channel", d.Name(), "err", err)
			exit(1)
		}
	}
	for _, d := range wecomDrivers {
		if err := d.Start(ctx); err != nil {
			log.Error("channel start failed", "channel", d.Name(), "err", err)
			exit(1)
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
	watcher.SetSupport(&reload.SupportSource{Paths: luacode.SupportChunkPaths(), Reload: sup.ReloadAll})
	watcher.SetPackages(&reload.PackageSource{Paths: luacode.PackagePaths(), Reload: sup.ReloadAll})
	watcher.Start()

	// The admin plane: health, readiness, metrics, the admin API and the
	// console. It is served on the shared HTTP listener (and on the optional
	// local socket); it has no port of its own.
	adminSocket := strings.TrimSpace(cfg.Runtime.Admin.Socket)
	if adminSocket != "" && stdruntime.GOOS != "windows" && !filepath.IsAbs(adminSocket) {
		adminSocket = filepath.Join(cfgSource.Dir, adminSocket)
	}
	tcpAdmin := cfg.Runtime.Admin.TCPEnabled()
	// Named operator tokens are resolved before the break-glass token decision:
	// "has operators" has to mean tokens that actually resolved, or a regional
	// cell whose every operator credential is missing would believe it has
	// authentication and mint nothing.
	ops := make(map[string]string, len(cfg.Runtime.Admin.Operators))
	for name, raw := range cfg.Runtime.Admin.Operators {
		v, ok := credResolver.Resolve(ctx, raw)
		if !ok {
			log.Warn("admin operator skipped: unresolved credential", "operator", name)
			continue
		}
		ops[name] = v
	}
	adminToken, adminLocalOnly, tokErr := resolveAdminToken(os.Getenv("ADMIN_TOKEN"), len(ops) > 0, cfg.IsRegional(), log)
	if tokErr != nil {
		log.Error("admin plane has no usable authentication", "err", tokErr)
		exit(1)
	}
	admin := metrics.NewAdminServer("", adminToken, metricReg, log)
	adminReady := admin.Ready
	readyFn.Store(&adminReady)
	admin.SetCORS(cfg.Runtime.Admin.CORSOrigins)
	if len(ops) > 0 {
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
	{
		// The full-fidelity user bundle: one person's profile, memory, files and
		// re-encrypted credentials, for moving them between regional cells.
		bundleSvc := &bundle.Service{
			Identity: identReg,
			Memory:   memMgr,
			Memories: agentMemories,
			Files:    filesMgr,
			Creds:    credStore,
			Tenancy:  tenReg,
			Region:   cfg.Runtime.Region,
			Log:      log,
		}
		var pluginsStore *studio.Plugins
		if cfg.Runtime.Studio.IsEnabled() {
			pluginsStore = studio.NewPlugins(cfg, cfgSource.ConfigDir(), cfgSource.ConfigPath(), studio.PluginHooks{
				OnReloadSupport: func() {
					sup.ReloadAll()
					watcher.RefreshSupport(luacode.SupportChunkPaths())
				},
				OnReloadRoute: func() {
					if routeRef == "plugin:per_chat" {
						if src, watchPath, err := luacode.Resolve("plugin:per_chat"); err == nil {
							rtr.Reload(src)
							watcher.RefreshRoute(watchPath)
						}
					}
				},
				OnReloadPackages: func() {
					sup.ReloadAll()
					watcher.RefreshPackages(luacode.PackagePaths())
				},
			}, log)
		}
		api := adminapi.New(adminapi.Deps{
			ConfigPath: cfgSource.ConfigPath(),
			ConfigDir:  cfgSource.ConfigDir(),
			Cfg:        cfg,
			Instance:   leaseMgr.Owner(),
			Region:     cfg.Runtime.Region,
			Topology:   cfg.Topology(),
			Zone:       cfg.Runtime.Zone,
			Instances:  fleetReg.List,
			Bundle:     bundleSvc,
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
			Memory:     memoryStore,
			Plugins:    pluginsStore,
			Media:      mediaSvc,
			StudioCatalog: func() studio.Catalog {
				curCfg := cfg
				if loaded, err := cfgSource.Load(nil); err == nil && loaded != nil {
					curCfg = loaded
				}
				models := make([]string, 0)
				for name := range llmMgr.List() {
					models = append(models, name)
				}
				safety := []string{"default", "none"}
				for name := range curCfg.Profiles.Safety {
					safety = append(safety, name)
				}
				memory := []string{"conversational"}
				for name := range curCfg.Profiles.Memory {
					memory = append(memory, name)
				}
				prompts := make([]string, 0)
				for name := range curCfg.Prompts {
					prompts = append(prompts, name)
				}
				cat := studio.NewCatalog(models, toolReg.Names(), safety, memory, prompts, config.DefaultCapabilities)
				for name := range curCfg.Profiles.Shell {
					cat.ShellProfiles = append(cat.ShellProfiles, name)
				}
				sort.Strings(cat.ShellProfiles)
				for name := range curCfg.Environments {
					cat.Environments = append(cat.Environments, name)
				}
				sort.Strings(cat.Environments)
				for name := range curCfg.Packages {
					cat.Packages = append(cat.Packages, name)
				}
				sort.Strings(cat.Packages)
				return cat
			},
			Users: adminapi.UserDeps{
				Identities: identReg,
				Store:      rtStore,
				Events:     eventLog,
				Journal:    journalSink,
				Quota:      userQuota,
				Files:      filesMgr,
				Media:      mediaSvc,
				Purge:      rtStore,
			},
		})
		admin.Mount("/admin/api/", api.API(), true)
		if tcpAdmin {
			log.Info("admin API enabled", "url", localURL(cfg.Gateway.Listen)+"/admin/api/")
		} else {
			log.Info("admin API HTTP disabled; serving on the local admin socket")
		}
	}
	if tcpAdmin {
		// Mounted after the listener is bound: the stdlib mux takes
		// registrations while serving, and binding first is what lets the
		// telegram auto probe run before this point.
		shared := admin.SharedHandler(adminLocalOnly)
		for _, p := range metrics.SharedPaths {
			httpSrv.Mount(p, shared)
		}
		where := "any client with the token"
		if adminLocalOnly {
			where = "local clients only"
		}
		log.Info("admin API on the shared listener", "listen", cfg.Gateway.Listen, "prefix", "/admin/", "reachable", where)
	}
	if adminSocket != "" {
		l, err := metrics.ListenLocalAdmin(adminSocket)
		if err != nil {
			log.Error("admin local listener failed", "socket", adminSocket, "err", err)
			exit(1)
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
	<-sigCtx.Done()
	// From here a second ctrl+c or SIGTERM takes the default action and kills
	// the process, so a drain that hangs can always be cut short.
	stop()

	log.Info("shutting down")
	// Report unready first, then drain: a load balancer that honours readiness
	// stops sending new traffic before the listeners close, and an in-flight
	// turn gets the window to finish. Everything keeps running on ctx during
	// the window — this is a drain, not a pause. A regional cell defaults to
	// five seconds; single keeps the old immediate exit. The triggers stop
	// first so their claims go to a peer now and the next scheduled fire is
	// not lost to the window.
	admin.SetReady(false)
	triggerSvc.Stop()
	if grace := cfg.ShutdownGrace(); grace > 0 {
		log.Info("draining before shutdown", "grace", grace.String())
		time.Sleep(grace)
	}
	cancelRun()
	// The actors unwind on ctx; let them finish before their leases go, or a
	// peer can claim a session while this instance is still ending its turn.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if !sup.Wait(waitCtx) {
		log.Warn("sessions still running after shutdown wait; releasing their leases anyway")
	}
	waitCancel()
	if telemetryExporter != nil {
		texCtx, texCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := telemetryExporter.Shutdown(texCtx); err != nil {
			log.Warn("telemetry exporter shutdown error", "err", err)
		}
		texCancel()
	}
	// Before the stores close: the hub gives up its sessions so the rest of the
	// fleet picks them up at once instead of waiting out the leases. ctx is
	// cancelled by now, and the store refuses a cancelled context, so the
	// release gets its own.
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if hub != nil {
		hub.Release(releaseCtx)
	}
	releaseCancel()
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

// warnTopologyConfig reports the topology mistakes that validation cannot see.
//
// The first is a fleet that never declared itself: a PostgreSQL runtime store
// with no runtime.topology is what every multi-instance deployment looked like
// before the tier existed, and it is exactly the configuration whose tier rules
// are not checked. The second is a regional cell whose identity or credential
// store is on a different PostgreSQL server from the runtime store: still
// shared by every instance, so not refused, but a different failure domain from
// the one the cell is designed around.
func startTelemetryExporter(ctx context.Context, tc config.TracingConfig, resolver *config.Resolver, log *slog.Logger) *telemetry.Exporter {
	pk := tc.PublicKey
	if pk != "" && resolver != nil {
		if v, ok := resolver.Resolve(ctx, pk); ok {
			pk = v
		} else {
			log.Warn("telemetry exporter public_key unresolved; exporter disabled", "credential", config.CredentialName(pk))
			return nil
		}
	}
	sk := tc.SecretKey
	if sk != "" && resolver != nil {
		if v, ok := resolver.Resolve(ctx, sk); ok {
			sk = v
		} else {
			log.Warn("telemetry exporter secret_key unresolved; exporter disabled", "credential", config.CredentialName(sk))
			return nil
		}
	}
	headers := make(map[string]string, len(tc.Headers))
	for k, raw := range tc.Headers {
		val := raw
		if val != "" && resolver != nil {
			if v, ok := resolver.Resolve(ctx, raw); ok {
				val = v
			} else {
				log.Warn("telemetry exporter header unresolved; exporter disabled", "header", k, "credential", config.CredentialName(raw))
				return nil
			}
		}
		headers[k] = val
	}
	var flushInterval, timeout time.Duration
	if strings.TrimSpace(tc.FlushInterval) != "" {
		flushInterval, _ = config.ParseEvery(tc.FlushInterval)
	}
	if strings.TrimSpace(tc.Timeout) != "" {
		timeout, _ = config.ParseEvery(tc.Timeout)
	}
	exp, err := telemetry.New(telemetry.Options{
		Provider:        tc.Provider,
		Endpoint:        tc.Endpoint,
		ServiceName:     tc.ServiceName,
		Headers:         headers,
		PublicKey:       pk,
		SecretKey:       sk,
		SampleRate:      tc.Sample(),
		IncludePayloads: tc.IncludePayloads,
		BatchSize:       tc.BatchSize,
		FlushInterval:   flushInterval,
		Timeout:         timeout,
	}, log)
	if err != nil {
		log.Warn("telemetry exporter disabled", "err", err)
		return nil
	}
	log.Info("telemetry tracing enabled", "provider", tc.Provider, "endpoint", tc.Endpoint, "payloads", tc.IncludePayloads)
	return exp
}

func warnTopologyConfig(cfg *config.Config, log *slog.Logger) {
	if strings.TrimSpace(cfg.Runtime.Topology) == "" && storedb.BackendFor(cfg.Runtime.Persistence) == storedb.BackendPostgres {
		log.Warn("runtime.persistence is PostgreSQL but runtime.topology is unset: this looks like a fleet; set runtime.topology: regional so the tier's requirements are checked")
	}
	if !cfg.IsRegional() {
		return
	}
	for _, t := range []struct {
		key    string
		target string
	}{
		{"runtime.identity.persistence", cfg.Runtime.Identity.Persistence},
		{"runtime.credentials.persistence", cfg.Runtime.Credentials.Persistence},
	} {
		if strings.TrimSpace(t.target) == "" {
			continue
		}
		if !config.SamePostgresServer(t.target, cfg.PersistencePath()) {
			log.Warn(t.key+" is on a different PostgreSQL server from runtime.persistence: every instance can still reach it, but it is a separate failure domain from the one this cell is built around",
				"setting", t.key, "target", storedb.Display(t.target))
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
	for name, a := range cfg.Agents {
		if a.Memory.IsInline {
			report("agent:"+name, a.Memory.Inline.Stores)
		}
	}
}

// localURL is the URL a client on this host dials for a listen address: an
// unspecified host (":8080", "0.0.0.0:8080") is reached on loopback.
func localURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// loopbackListen reports whether an admin listen address is reachable only
// from this host, which is what makes a per-boot token a secret worth having.
//
// It is deliberately strict, and answers from the address text alone rather
// than resolving anything: an empty host (":8080") binds every interface, a
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
	return config.LoopbackListen(addr)
}

// resolveAdminToken decides how the admin plane authenticates. It returns the
// break-glass bearer token ("" when named operators are the only credentials)
// and whether that token was minted for this boot.
//
// A token is always required: one is minted whenever none is configured. That
// used to depend on the console being on, and `-no-webui` with no ADMIN_TOKEN
// left an empty token, which metrics.AdminServer.auth reads as "no auth": the
// admin routes were served to anything that could reach them.
//
// A minted token is written to the log, so it is not a secret from anyone who
// can read the log; the caller serves the admin plane local-only while minted
// is true. A configured credential (ADMIN_TOKEN, or named operators) is what
// opens the admin plane to remote clients. With operators and no ADMIN_TOKEN,
// nothing is minted: a logged break-glass token beside them would be the
// weakest credential on a reachable port.
//
// A regional cell shares one admin credential. A per-boot token is unknown to
// every peer, so regional refuses to mint: ADMIN_TOKEN or operators it is.
// envToken is a parameter rather than read here so every branch is testable.
func resolveAdminToken(envToken string, hasOperators, regional bool, log *slog.Logger) (token string, minted bool, err error) {
	if log == nil {
		log = slog.Default()
	}
	if envToken != "" {
		return envToken, false, nil
	}
	if hasOperators {
		log.Info("admin authentication uses runtime.admin.operators; no break-glass token minted")
		return "", false, nil
	}
	if regional {
		return "", false, fmt.Errorf("regional topology: ADMIN_TOKEN is unset and no runtime.admin.operators resolved; every instance would mint its own per-boot token and reject the others'. Set ADMIN_TOKEN or name runtime.admin.operators")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", false, fmt.Errorf("admin token generation: %w", err)
	}
	token = hex.EncodeToString(b)
	log.Info("admin token (set ADMIN_TOKEN to pin it)", "token", token)
	log.Warn("the admin API answers only local clients while its token is minted per boot; set ADMIN_TOKEN or runtime.admin.operators to reach it remotely")
	return token, true, nil
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
	// The file tools are forbidden with them: they read and write the same
	// project the shell tools run commands in.
	out.Forbidden = append(append([]string{}, policy.Forbidden...),
		"builtin:shell.exec", "builtin:shell.write", "builtin:shell.destroy",
		"builtin:fs.read", "builtin:fs.write")
	return out
}

// sandboxInUse reports whether any agent or spawn profile is bound to a
// sandbox shell profile, which is when it is worth finding out at boot
// whether the sandbox works here.
func sandboxInUse(cfg *config.Config) bool {
	uses := func(name string) bool {
		p, ok := cfg.Profiles.Shell[name]
		return ok && p.Provider != "ssh"
	}
	for _, a := range cfg.Agents {
		if uses(a.Shell) {
			return true
		}
	}
	for _, p := range cfg.Profiles.Agent {
		if uses(p.Shell) {
			return true
		}
	}
	return false
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
		if lname, ok := strings.CutPrefix(name, "lua:"); ok {
			profile, exists := cfg.Safety.LuaFilters[lname]
			if !exists {
				return nil, fmt.Errorf("safety profile %q references lua filter %q, which is not configured", ref, lname)
			}
			lf, err := safety.NewLuaFilter(luaFilterOptions(lname, profile))
			if err != nil {
				return nil, fmt.Errorf("safety profile %q lua filter %q: %w", ref, lname, err)
			}
			filters = append(filters, lf)
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

func luaFilterOptions(name string, p config.LuaFilterProfile) safety.LuaFilterOptions {
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
	return safety.LuaFilterOptions{
		Name:        name,
		Source:      p.EffectiveSource(),
		Ingress:     ingress,
		Egress:      egress,
		Sources:     p.Sources,
		Timeout:     p.TimeoutD(),
		InstrBudget: p.InstrBudget,
		OnError:     p.OnError,
		Refusal:     p.Refusal,
		Fallback:    p.Fallback,
	}
}

// budgetTokens returns the agent's configured token budget.
func budgetTokens(a config.Agent) int64 {
	return a.Budget.TokensPerDay
}

// budgetWindow parses the optional rolling-window duration from the agent's
// budget config (e.g. budget: { tokens_per_day: N, window: "168h" }). Returns 0
// when unset, which selects the daily-reset accounting mode.
func budgetWindow(a config.Agent) time.Duration {
	if a.Budget.Window == "" {
		return 0
	}
	d, err := time.ParseDuration(a.Budget.Window)
	if err != nil {
		return 0
	}
	return d
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
	// Both were validated at load, so neither parse can fail here.
	timeout, _ := time.ParseDuration(p.Timeout)
	var memory int64
	if p.MemLimit != "" {
		memory, _ = config.ParseByteSize(p.MemLimit)
	}
	return map[string]any{
		"provider":     p.Provider,
		"environment":  p.Environment,
		"project":      p.Project,
		"workdir":      p.WorkDir,
		"network":      p.Network,
		"timeout":      timeout,
		"ignore":       p.Ignore,
		"memory_bytes": memory,
		"cpu_limit":    p.CPULimit,
		"env":          p.Env,
		"host":         p.Host,
		"user":         p.User,
		"password":     p.Password,
		"key_file":     p.KeyFile,
	}
}

// useSessionHub reports whether this process installs the session hub. A
// regional cell always does: the hub is how a message that lands on the wrong
// instance reaches the one that owns the session. A single instance does not,
// because there is no other owner to hand it to and the lease would be a row
// per conversation bought for nothing. -force-single is the exception: an
// operator using it is saying two processes share the store, and then the lease
// is the only thing keeping one conversation from running twice.
func useSessionHub(cfg *config.Config, forceSingle bool) bool {
	return cfg.IsRegional() || forceSingle
}
