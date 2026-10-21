package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/budget"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/safety"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/llm"
)

// UsageRecorder matches caps.UsageRecorder without importing the caps package.
type UsageRecorder interface {
	RecordUsage(rec runtime.UsageRecord, withEvent bool) error
}

// Options configures one model-backed classifier. Labels are the allowed
// output vocabulary; the policy that maps them to actions lives in the safety
// filter.
type Options struct {
	Name           string
	Model          string
	Prompt         string
	Labels         []string
	MaxTokens      int
	MaxConcurrency int
	CacheSize      int
	CacheTTL       time.Duration
	Pool           *budget.Pool
	Ledger         UsageRecorder
	Events         bool
	LedgerAgent    string // synthetic ledger bucket, e.g. "__moderation_moderation"
	Log            *slog.Logger
}

// Client is a chat-model classifier. It is safe for concurrent use; a
// semaphore bounds in-flight provider calls and a small breaker stops a down
// provider from turning every turn into a timeout.
type Client struct {
	name        string
	model       string
	prompt      string
	promptHash  string
	labels      map[string]string // lower-case -> canonical
	labelList   []string
	maxTokens   int
	mgr         *llm.Manager
	sem         chan struct{}
	cache       *verdictCache
	pool        *budget.Pool
	ledger      UsageRecorder
	events      bool
	ledgerAgent string
	log         *slog.Logger
	breaker     breaker
}

type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

const (
	breakerThreshold = 5
	breakerCooldown  = 30 * time.Second
)

// New validates a classifier definition. Config validation has already
// checked the model and prompt references; this catches construction errors
// that would otherwise surface on the first turn.
func New(mgr *llm.Manager, opts Options) (*Client, error) {
	if mgr == nil {
		return nil, errors.New("moderation: nil llm manager")
	}
	if strings.TrimSpace(opts.Model) == "" {
		return nil, errors.New("moderation: model is required")
	}
	if len(opts.Labels) == 0 {
		return nil, errors.New("moderation: at least one label is required")
	}
	labels := make(map[string]string, len(opts.Labels))
	list := make([]string, 0, len(opts.Labels))
	for _, l := range opts.Labels {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		labels[strings.ToLower(l)] = l
		list = append(list, l)
	}
	if len(labels) == 0 {
		return nil, errors.New("moderation: labels must not be empty")
	}
	sort.Strings(list)
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64
	}
	prompt := opts.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = DefaultPrompt
	}
	sum := sha256.Sum256([]byte(prompt + "\x00" + strings.Join(list, ",")))
	c := &Client{
		name:        opts.Name,
		model:       opts.Model,
		prompt:      prompt,
		promptHash:  hex.EncodeToString(sum[:]),
		labels:      labels,
		labelList:   list,
		maxTokens:   maxTokens,
		mgr:         mgr,
		cache:       newVerdictCache(opts.CacheSize, opts.CacheTTL),
		pool:        opts.Pool,
		ledger:      opts.Ledger,
		events:      opts.Events,
		ledgerAgent: opts.LedgerAgent,
		log:         opts.Log,
	}
	if c.ledgerAgent == "" {
		c.ledgerAgent = "__moderation"
	}
	if opts.MaxConcurrency > 0 {
		c.sem = make(chan struct{}, opts.MaxConcurrency)
	}
	return c, nil
}

// Classify returns a raw verdict. The safety filter applies thresholds and
// label actions; this method only knows the model, the cache, and metering.
func (c *Client) Classify(ctx context.Context, req safety.ClassifyRequest) (safety.Verdict, error) {
	key := c.cacheKey(req.Text)
	if v, ok := c.cache.get(key); ok {
		v.Cached = true
		v.Latency = 0
		metrics.Inc("agentflow_moderation_cache_hits")
		return v, nil
	}
	if !c.breaker.allow() {
		return safety.Verdict{}, safety.ErrClassifierUnavailable
	}
	if c.sem != nil {
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		default:
			return safety.Verdict{}, safety.ErrClassifierSaturated
		}
	}

	msgs, err := buildMessages(c.prompt, c.labelList, req)
	if err != nil {
		return safety.Verdict{}, err
	}
	estimate := int64((len(c.prompt) + len(req.Text)) / 4)
	if estimate < 128 {
		estimate = 128
	}
	if c.pool != nil {
		c.pool.Refresh(ctx)
	}
	lease, err := c.reserve(estimate)
	if err != nil {
		return safety.Verdict{}, fmt.Errorf("%w: %v", safety.ErrClassifierBudget, err)
	}

	started := time.Now()
	zero := 0.0
	reply, err := c.mgr.Chat(ctx, c.model, msgs, llm.Opts{
		Temperature: &zero,
		MaxTokens:   c.maxTokens,
		Thinking:    "off",
	})
	latency := time.Since(started)
	if err != nil {
		c.release(lease)
		c.breaker.failure()
		c.record(ctx, req.Agent, llm.Usage{}, false)
		if c.log != nil {
			c.log.Warn("moderation: classifier call failed", "classifier", c.name, "model", c.model, "err", err)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return safety.Verdict{}, context.DeadlineExceeded
		}
		return safety.Verdict{}, err
	}

	actual := int64(reply.Usage.Input + reply.Usage.Output)
	c.commit(lease, actual)
	c.record(ctx, req.Agent, reply.Usage, true)
	metrics.Inc("agentflow_moderation_calls")
	metrics.Add("agentflow_moderation_tokens", actual)

	v, err := parseVerdict(reply.Text, c.labels)
	if err != nil {
		c.breaker.failure()
		if c.log != nil {
			c.log.Warn("moderation: invalid classifier output", "classifier", c.name, "model", c.model, "err", err)
		}
		return safety.Verdict{}, err
	}
	c.breaker.success()
	v.Model = c.model
	v.InputTokens = reply.Usage.Input
	v.OutputTokens = reply.Usage.Output
	v.Latency = latency
	c.cache.put(key, v)
	return v, nil
}

func (c *Client) cacheKey(text string) string {
	sum := sha256.Sum256([]byte(c.model + "\x00" + c.promptHash + "\x00" + text))
	return hex.EncodeToString(sum[:])
}

func (c *Client) reserve(amount int64) (*budget.Lease, error) {
	if c.pool == nil {
		return nil, nil
	}
	return c.pool.Reserve(amount)
}

func (c *Client) release(l *budget.Lease) {
	if c.pool != nil && l != nil {
		c.pool.Release(l)
	}
}

func (c *Client) commit(l *budget.Lease, actual int64) {
	if c.pool != nil && l != nil {
		if err := c.pool.Commit(l, actual); err != nil && c.log != nil {
			c.log.Warn("moderation: budget commit failed", "classifier", c.name, "err", err)
		}
	}
}

// record writes one ledger row. The synthetic ledger agent keeps moderation
// spend out of the agent's own daily budget; the real agent and the user are
// still visible in logs and the usage event row.
func (c *Client) record(ctx context.Context, agent string, u llm.Usage, ok bool) {
	if c.ledger == nil {
		return
	}
	rec := runtime.UsageRecord{
		UserID: session.BillingUUIDFromCtx(ctx),
		Agent:  c.ledgerAgent,
		Model:  c.model,
		Kind:   "moderation",
		Input:  u.Input,
		Output: u.Output,
		Cached: u.Cached,
		OK:     ok,
	}
	if agent != "" && c.log != nil {
		c.log.Debug("moderation: usage", "classifier", c.name, "agent", agent, "input", u.Input, "output", u.Output)
	}
	if err := c.ledger.RecordUsage(rec, c.events); err != nil {
		metrics.Inc("agentflow_usage_record_failed")
		if c.log != nil {
			c.log.Warn("moderation: usage record failed", "classifier", c.name, "err", err)
		}
	}
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if time.Now().Before(b.openUntil) {
		return false
	}
	// Half-open: let the next call through. A success closes it; a failure
	// re-opens for another cooldown.
	b.openUntil = time.Time{}
	b.failures = 0
	return true
}

func (b *breaker) success() {
	b.mu.Lock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.mu.Unlock()
}

func (b *breaker) failure() {
	b.mu.Lock()
	b.failures++
	if b.failures >= breakerThreshold {
		b.openUntil = time.Now().Add(breakerCooldown)
		b.failures = 0
	}
	b.mu.Unlock()
}
