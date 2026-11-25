// Package email implements an IMAP (inbound) + SMTP (outbound) channel driver.
//
// Inbound emails are polled or watched via IMAP IDLE, filtered through optional
// exact-address and domain allowlists, grouped into one session per email thread
// (References / In-Reply-To / Message-ID), optionally marked \Seen, and have
// attachments ingested into the media store. Outbound replies send threaded
// RFC 5322 / MIME messages over SMTP with In-Reply-To and References headers.
package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	netmail "net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
)

const (
	defaultIMAPPort       = 993
	defaultSMTPPort       = 465
	defaultMailbox        = "INBOX"
	defaultPollInterval   = 30 * time.Second
	maxIdleWait           = 25 * time.Minute
	connectTimeout        = 15 * time.Second
	smtpSendTimeout       = 60 * time.Second
	defaultMaxMediaBytes  = 8 << 20
	maxOutboundMediaBytes = 20 << 20
	maxBodyBytes          = 256 * 1024
	maxRawEmailBytes      = 32 << 20
	maxSeenUIDs           = 8192
	maxTrackedThreads     = 2048
)

// Options configures an IMAP + SMTP email channel driver.
type Options struct {
	IMAPHost     string
	IMAPPort     int
	SMTPHost     string
	SMTPPort     int
	Username     string
	Password     string
	SMTPUsername string
	SMTPPassword string
	FromAddress  string
	Mailbox      string
	Mode         string // "idle" (default) | "polling"
	PollInterval time.Duration
	MarkSeen     bool
	AllowFrom    []string
	AllowDomains []string
	Media        *media.Service

	// Optional test hooks and TLS override.
	TLSConfig *tls.Config
	DialIMAP  func(ctx context.Context, addr string, port int, opts *imapclient.Options) (*imapclient.Client, error)
	DialSMTP  func(ctx context.Context, host, addr string, port int, tlsCfg *tls.Config) (*smtp.Client, error)
}

// Driver connects AgentFlow to an IMAP/SMTP mailbox.
type Driver struct {
	name         string
	agent        string
	imapHost     string
	imapPort     int
	smtpHost     string
	smtpPort     int
	username     string
	password     string
	smtpUser     string
	smtpPass     string
	fromHeader   string
	fromAddr     string
	mailbox      string
	mode         string
	pollInterval time.Duration
	markSeen     bool

	allowFrom    map[string]bool
	allowDomains map[string]bool
	ownAddrs     map[string]bool

	media *media.Service
	store media.Store
	pol   media.Policy
	sink  router.Sink
	log   *slog.Logger

	tlsConfig *tls.Config
	dialIMAP  func(ctx context.Context, addr string, port int, opts *imapclient.Options) (*imapclient.Client, error)
	dialSMTP  func(ctx context.Context, host, addr string, port int, tlsCfg *tls.Config) (*smtp.Client, error)

	// OnDrop is invoked when an inbound email is blocked by the sender/domain allowlist.
	OnDrop func(channel, agent, chatID, sender, text, reason string)

	seenMu    sync.Mutex
	seen      map[string]bool
	seenOrder []string

	threadMu    sync.Mutex
	threads     map[string]*threadState
	threadOrder []string

	cancelMu sync.Mutex
	cancel   context.CancelFunc
}

type threadState struct {
	toAddr     string
	toName     string
	subject    string
	lastMsgID  string
	references []string
}

// ConnectionInfo reports live IMAP + SMTP connectivity and capability details.
type ConnectionInfo struct {
	IMAPHost    string `json:"imap_host"`
	IMAPPort    int    `json:"imap_port"`
	SMTPHost    string `json:"smtp_host"`
	SMTPPort    int    `json:"smtp_port"`
	Username    string `json:"username"`
	Mailbox     string `json:"mailbox"`
	Messages    uint32 `json:"messages"`
	IDLESupport bool   `json:"idle_support"`
	IMAPOK      bool   `json:"imap_ok"`
	SMTPOK      bool   `json:"smtp_ok"`
}

// TestOptions configures a one-shot TestConnection probe.
type TestOptions struct {
	IMAPHost     string
	IMAPPort     int
	SMTPHost     string
	SMTPPort     int
	Username     string
	Password     string
	SMTPUsername string
	SMTPPassword string
	Mailbox      string
	TLSConfig    *tls.Config
	DialIMAP     func(ctx context.Context, addr string, port int, opts *imapclient.Options) (*imapclient.Client, error)
	DialSMTP     func(ctx context.Context, host, addr string, port int, tlsCfg *tls.Config) (*smtp.Client, error)
}

// New constructs an email channel driver.
func New(name, agent string, opts Options, sink router.Sink, store media.Store, pol media.Policy, log *slog.Logger) *Driver {
	if log == nil {
		log = slog.Default()
	}
	imapPort := opts.IMAPPort
	if imapPort <= 0 {
		imapPort = defaultIMAPPort
	}
	smtpPort := opts.SMTPPort
	if smtpPort <= 0 {
		smtpPort = defaultSMTPPort
	}
	mailbox := strings.TrimSpace(opts.Mailbox)
	if mailbox == "" {
		mailbox = defaultMailbox
	}
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = "idle"
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	user := strings.TrimSpace(opts.Username)
	smtpUser := strings.TrimSpace(opts.SMTPUsername)
	if smtpUser == "" {
		smtpUser = user
	}
	smtpPass := opts.SMTPPassword
	if smtpPass == "" {
		smtpPass = opts.Password
	}
	fromHeader := strings.TrimSpace(opts.FromAddress)
	fromAddr := smtpUser
	if fromHeader != "" {
		if parsed, err := netmail.ParseAddress(fromHeader); err == nil && parsed.Address != "" {
			fromAddr = parsed.Address
		}
	} else {
		fromHeader = smtpUser
	}

	allowFrom := make(map[string]bool, len(opts.AllowFrom))
	for _, a := range opts.AllowFrom {
		s := strings.ToLower(strings.TrimSpace(a))
		if s != "" {
			allowFrom[s] = true
		}
	}
	allowDomains := make(map[string]bool, len(opts.AllowDomains))
	for _, dom := range opts.AllowDomains {
		s := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(dom), "@"))
		if s != "" {
			allowDomains[s] = true
		}
	}
	ownAddrs := map[string]bool{}
	for _, raw := range []string{user, smtpUser, fromAddr} {
		s := strings.ToLower(strings.TrimSpace(raw))
		if strings.Contains(s, "@") {
			ownAddrs[s] = true
		}
	}

	return &Driver{
		name:         name,
		agent:        agent,
		imapHost:     strings.TrimSpace(opts.IMAPHost),
		imapPort:     imapPort,
		smtpHost:     strings.TrimSpace(opts.SMTPHost),
		smtpPort:     smtpPort,
		username:     user,
		password:     opts.Password,
		smtpUser:     smtpUser,
		smtpPass:     smtpPass,
		fromHeader:   fromHeader,
		fromAddr:     strings.ToLower(strings.TrimSpace(fromAddr)),
		mailbox:      mailbox,
		mode:         mode,
		pollInterval: poll,
		markSeen:     opts.MarkSeen,
		allowFrom:    allowFrom,
		allowDomains: allowDomains,
		ownAddrs:     ownAddrs,
		media:        opts.Media,
		store:        store,
		pol:          pol,
		sink:         sink,
		log:          log.With("driver", "email", "channel", name, "mode", mode),
		tlsConfig:    opts.TLSConfig,
		dialIMAP:     opts.DialIMAP,
		dialSMTP:     opts.DialSMTP,
		seen:         map[string]bool{},
		threads:      map[string]*threadState{},
	}
}

// Name returns the configured channel name.
func (d *Driver) Name() string { return d.name }

// Start validates configuration and launches the background IMAP monitor loop.
func (d *Driver) Start(ctx context.Context) error {
	if d.imapHost == "" || d.smtpHost == "" || d.username == "" || d.password == "" {
		return errors.New("email: imap_host, smtp_host, username, and password are required")
	}
	runCtx, cancel := context.WithCancel(ctx)
	d.cancelMu.Lock()
	d.cancel = cancel
	d.cancelMu.Unlock()

	go d.runLoop(runCtx)
	d.log.Info("email channel started", "imap_host", d.imapHost, "smtp_host", d.smtpHost, "mailbox", d.mailbox, "mark_seen", d.markSeen)
	return nil
}

// Stop stops the background IMAP loop.
func (d *Driver) Stop(_ context.Context) {
	d.cancelMu.Lock()
	cancel := d.cancel
	d.cancel = nil
	d.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *Driver) isAllowed(addr string) bool {
	if len(d.allowFrom) == 0 && len(d.allowDomains) == 0 {
		return true
	}
	norm := strings.ToLower(strings.TrimSpace(addr))
	if norm == "" {
		return false
	}
	if d.allowFrom[norm] {
		return true
	}
	if _, domain, ok := strings.Cut(norm, "@"); ok && domain != "" {
		return d.allowDomains[domain]
	}
	return false
}

func (d *Driver) markSeenUID(uidValidity uint32, uid imap.UID) bool {
	key := fmt.Sprintf("%d:%d", uidValidity, uid)
	d.seenMu.Lock()
	defer d.seenMu.Unlock()
	if d.seen[key] {
		return false
	}
	d.seen[key] = true
	d.seenOrder = append(d.seenOrder, key)
	for len(d.seenOrder) > maxSeenUIDs {
		old := d.seenOrder[0]
		d.seenOrder = d.seenOrder[1:]
		delete(d.seen, old)
	}
	return true
}

func (d *Driver) hasSeenUID(uidValidity uint32, uid imap.UID) bool {
	key := fmt.Sprintf("%d:%d", uidValidity, uid)
	d.seenMu.Lock()
	defer d.seenMu.Unlock()
	return d.seen[key]
}

func (d *Driver) upsertThread(threadID, toAddr, toName, subject, msgID string, refs []string) {
	if threadID == "" {
		return
	}
	d.threadMu.Lock()
	defer d.threadMu.Unlock()
	st, ok := d.threads[threadID]
	if !ok {
		st = &threadState{}
		d.threads[threadID] = st
		d.threadOrder = append(d.threadOrder, threadID)
		for len(d.threadOrder) > maxTrackedThreads {
			old := d.threadOrder[0]
			d.threadOrder = d.threadOrder[1:]
			delete(d.threads, old)
		}
	}
	if toAddr != "" {
		st.toAddr = toAddr
		st.toName = toName
	}
	if subject != "" {
		st.subject = subject
	}
	if msgID != "" {
		st.lastMsgID = msgID
	}
	for _, r := range refs {
		appendUniqueID(&st.references, r)
	}
	if msgID != "" {
		appendUniqueID(&st.references, msgID)
	}
}

func (d *Driver) threadSnapshot(threadID string) (threadState, bool) {
	d.threadMu.Lock()
	defer d.threadMu.Unlock()
	st, ok := d.threads[threadID]
	if !ok {
		return threadState{}, false
	}
	cp := *st
	cp.references = append([]string(nil), st.references...)
	return cp, true
}

func appendUniqueID(list *[]string, raw string) {
	id := normalizeMsgID(raw)
	if id == "" {
		return
	}
	for _, existing := range *list {
		if existing == id {
			return
		}
	}
	*list = append(*list, id)
}

func normalizeMsgID(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}

func scrubSecrets(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, sec := range secrets {
		if sec != "" {
			msg = strings.ReplaceAll(msg, sec, "[redacted]")
		}
	}
	return errors.New(msg)
}

// TestConnection verifies both IMAP login/mailbox selection and SMTP
// authentication without altering mailbox flags or sending a message.
func TestConnection(ctx context.Context, opts TestOptions) (ConnectionInfo, error) {
	imapHost := strings.TrimSpace(opts.IMAPHost)
	smtpHost := strings.TrimSpace(opts.SMTPHost)
	username := strings.TrimSpace(opts.Username)
	password := opts.Password
	if imapHost == "" {
		return ConnectionInfo{}, errors.New("email test: imap_host is required")
	}
	if smtpHost == "" {
		return ConnectionInfo{}, errors.New("email test: smtp_host is required")
	}
	if username == "" {
		return ConnectionInfo{}, errors.New("email test: username is required")
	}
	if password == "" {
		return ConnectionInfo{}, errors.New("email test: password is required")
	}
	imapPort := opts.IMAPPort
	if imapPort <= 0 {
		imapPort = defaultIMAPPort
	}
	smtpPort := opts.SMTPPort
	if smtpPort <= 0 {
		smtpPort = defaultSMTPPort
	}
	mailbox := strings.TrimSpace(opts.Mailbox)
	if mailbox == "" {
		mailbox = defaultMailbox
	}
	smtpUser := strings.TrimSpace(opts.SMTPUsername)
	if smtpUser == "" {
		smtpUser = username
	}
	smtpPass := opts.SMTPPassword
	if smtpPass == "" {
		smtpPass = password
	}

	info := ConnectionInfo{
		IMAPHost: imapHost,
		IMAPPort: imapPort,
		SMTPHost: smtpHost,
		SMTPPort: smtpPort,
		Username: username,
		Mailbox:  mailbox,
	}

	tctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	// 1. Test IMAP login and mailbox select.
	imapAddr := net.JoinHostPort(imapHost, strconv.Itoa(imapPort))
	tlsCfg := cloneTLSConfig(imapHost, opts.TLSConfig)
	imapOpts := &imapclient.Options{
		TLSConfig: tlsCfg,
		Dialer:    &net.Dialer{Timeout: connectTimeout},
	}
	dialIMAP := opts.DialIMAP
	if dialIMAP == nil {
		dialIMAP = defaultDialIMAP
	}
	ic, err := dialIMAP(tctx, imapAddr, imapPort, imapOpts)
	if err != nil {
		return info, scrubSecrets(fmt.Errorf("email IMAP connect %s: %w", imapAddr, err), password, smtpPass)
	}
	defer ic.Close()

	if err := ic.Login(username, password).Wait(); err != nil {
		return info, scrubSecrets(fmt.Errorf("email IMAP login failed: %w", err), password, smtpPass)
	}
	info.IDLESupport = ic.Caps().Has(imap.CapIdle)
	sel, err := ic.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return info, scrubSecrets(fmt.Errorf("email IMAP select %q failed: %w", mailbox, err), password, smtpPass)
	}
	info.Messages = sel.NumMessages
	info.IMAPOK = true
	_ = ic.Logout().Wait()

	// 2. Test SMTP connection + authentication.
	smtpAddr := net.JoinHostPort(smtpHost, strconv.Itoa(smtpPort))
	smtpTLS := cloneTLSConfig(smtpHost, opts.TLSConfig)
	dialSMTP := opts.DialSMTP
	if dialSMTP == nil {
		dialSMTP = defaultDialSMTP
	}
	sc, err := dialSMTP(tctx, smtpHost, smtpAddr, smtpPort, smtpTLS)
	if err != nil {
		return info, scrubSecrets(fmt.Errorf("email SMTP connect %s: %w", smtpAddr, err), password, smtpPass)
	}
	defer sc.Close()

	if err := smtpAuthenticate(sc, smtpUser, smtpPass, smtpHost); err != nil {
		return info, scrubSecrets(fmt.Errorf("email SMTP auth failed: %w", err), password, smtpPass)
	}
	if err := sc.Noop(); err != nil {
		return info, scrubSecrets(fmt.Errorf("email SMTP noop failed: %w", err), password, smtpPass)
	}
	_ = sc.Quit()
	info.SMTPOK = true
	return info, nil
}

func cloneTLSConfig(host string, base *tls.Config) *tls.Config {
	if base != nil {
		cp := base.Clone()
		if cp.ServerName == "" {
			cp.ServerName = host
		}
		return cp
	}
	return &tls.Config{ServerName: host}
}

func defaultDialIMAP(ctx context.Context, addr string, port int, opts *imapclient.Options) (*imapclient.Client, error) {
	if opts == nil {
		opts = &imapclient.Options{}
	}
	var resErr error
	var client *imapclient.Client
	done := make(chan struct{})
	go func() {
		defer close(done)
		if port == 143 {
			client, resErr = imapclient.DialStartTLS(addr, opts)
		} else {
			client, resErr = imapclient.DialTLS(addr, opts)
		}
	}()
	select {
	case <-done:
		return client, resErr
	case <-ctx.Done():
		// Let the goroutine close any late connection if ctx aborted first.
		go func() {
			<-done
			if client != nil {
				_ = client.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func defaultDialSMTP(ctx context.Context, host, addr string, port int, tlsCfg *tls.Config) (*smtp.Client, error) {
	dialer := &net.Dialer{Timeout: connectTimeout}
	if port == 465 {
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: cloneTLSConfig(host, tlsCfg)}
		conn, err := tlsDialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		c, err := smtp.NewClient(conn, host)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return c, nil
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(cloneTLSConfig(host, tlsCfg)); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
	}
	return c, nil
}

func smtpAuthenticate(client *smtp.Client, user, password, host string) error {
	ok, mechs := client.Extension("AUTH")
	if !ok {
		return client.Auth(smtp.PlainAuth("", user, password, host))
	}
	upper := strings.ToUpper(mechs)
	if strings.Contains(upper, "PLAIN") || !strings.Contains(upper, "LOGIN") {
		err := client.Auth(smtp.PlainAuth("", user, password, host))
		if err == nil || !strings.Contains(upper, "LOGIN") {
			return err
		}
	}
	return client.Auth(&loginAuth{username: user, password: password})
}

type loginAuth struct {
	username string
	password string
}

func (a *loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", []byte(a.username), nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	prompt := strings.ToLower(strings.TrimSpace(string(fromServer)))
	switch {
	case strings.Contains(prompt, "user"):
		return []byte(a.username), nil
	case strings.Contains(prompt, "pass"):
		return []byte(a.password), nil
	default:
		return []byte(a.password), nil
	}
}
