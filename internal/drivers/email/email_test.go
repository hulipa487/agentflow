package email

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
)

type captureSink struct {
	mu   sync.Mutex
	inbs []router.Inbound
}

func (s *captureSink) Submit(in router.Inbound) {
	s.mu.Lock()
	s.inbs = append(s.inbs, in)
	s.mu.Unlock()
}

func (s *captureSink) list() []router.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]router.Inbound(nil), s.inbs...)
}

type literalReader struct {
	*strings.Reader
	size int64
}

func (r literalReader) Size() int64 { return r.size }

func startMockIMAP(t *testing.T, user, pass string, rawMessages ...string) (addr string, closeFn func()) {
	t.Helper()
	mem := imapmemserver.New()
	u := imapmemserver.NewUser(user, pass)
	mem.AddUser(u)
	if err := u.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	for _, raw := range rawMessages {
		lit := literalReader{
			Reader: strings.NewReader(raw),
			size:   int64(len(raw)),
		}
		if _, err := u.Append("INBOX", lit, &imap.AppendOptions{Time: time.Now()}); err != nil {
			t.Fatalf("append mock mail: %v", err)
		}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIdle:      {},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen imap: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() {
		_ = srv.Close()
		_ = ln.Close()
	}
}

type mockSMTPRecord struct {
	from string
	to   []string
	data string
}

type mockSMTPServer struct {
	ln       net.Listener
	expected string
	mu       sync.Mutex
	sent     []mockSMTPRecord
}

func startMockSMTP(t *testing.T, expectedUser, expectedPass string) *mockSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen smtp: %v", err)
	}
	s := &mockSMTPServer{
		ln:       ln,
		expected: expectedUser + "\x00" + expectedPass,
	}
	go s.serve()
	return s
}

func (s *mockSMTPServer) addr() string { return s.ln.Addr().String() }
func (s *mockSMTPServer) close()       { _ = s.ln.Close() }

func (s *mockSMTPServer) records() []mockSMTPRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mockSMTPRecord(nil), s.sent...)
}

func (s *mockSMTPServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(c)
	}
}

func (s *mockSMTPServer) handleConn(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	write := func(line string) { _, _ = io.WriteString(c, line+"\r\n") }

	write("220 mock-smtp ESMTP")
	authed := false
	var rec mockSMTPRecord
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			write("250-mock-smtp")
			write("250 AUTH PLAIN LOGIN")
		case strings.HasPrefix(upper, "AUTH PLAIN "):
			payload := strings.TrimSpace(line[len("AUTH PLAIN "):])
			raw, decErr := base64.StdEncoding.DecodeString(payload)
			if decErr != nil {
				write("535 invalid base64")
				continue
			}
			// \x00user\x00pass -> strip optional authzid before first null byte.
			parts := bytes.Split(raw, []byte{0})
			if len(parts) == 3 && string(parts[1])+"\x00"+string(parts[2]) == s.expected {
				authed = true
				write("235 Authentication successful")
			} else {
				write("535 Authentication credentials invalid")
			}
		case upper == "NOOP":
			write("250 Ok")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			if !authed {
				write("530 Authentication required")
				continue
			}
			rec = mockSMTPRecord{from: strings.TrimSpace(line[len("MAIL FROM:"):])}
			write("250 Ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			rec.to = append(rec.to, strings.TrimSpace(line[len("RCPT TO:"):]))
			write("250 Ok")
		case upper == "DATA":
			write("354 End data with <CR><LF>.<CR><LF>")
			var body strings.Builder
			for {
				dline, derr := r.ReadString('\n')
				if derr != nil {
					return
				}
				if strings.TrimRight(dline, "\r\n") == "." {
					break
				}
				body.WriteString(dline)
			}
			rec.data = body.String()
			s.mu.Lock()
			s.sent = append(s.sent, rec)
			s.mu.Unlock()
			write("250 Ok queued")
		case upper == "QUIT":
			write("221 Bye")
			return
		default:
			write("502 Unimplemented")
		}
	}
}

func dialPlainIMAPForTest(addr string) func(context.Context, string, int, *imapclient.Options) (*imapclient.Client, error) {
	return func(_ context.Context, _ string, _ int, opts *imapclient.Options) (*imapclient.Client, error) {
		return imapclient.DialInsecure(addr, opts)
	}
}

func dialPlainSMTPForTest(addr string) func(context.Context, string, string, int, *tls.Config) (*smtp.Client, error) {
	return func(_ context.Context, host, _ string, _ int, _ *tls.Config) (*smtp.Client, error) {
		return smtp.Dial(addr)
	}
}

func TestAllowlistMatching(t *testing.T) {
	dOpen := New("mail", "bot", Options{}, nil, nil, media.Policy{}, nil)
	if !dOpen.isAllowed("anyone@random.example") {
		t.Fatal("empty allowlists should permit all senders")
	}

	dFiltered := New("mail", "bot", Options{
		AllowFrom:    []string{"Alice@Partner.org"},
		AllowDomains: []string{"@Allowed.io", "corp.example"},
	}, nil, nil, media.Policy{}, nil)

	for _, okAddr := range []string{"alice@partner.org", "bob@allowed.io", "CAROL@CORP.EXAMPLE"} {
		if !dFiltered.isAllowed(okAddr) {
			t.Fatalf("expected %q to pass allowlist", okAddr)
		}
	}
	for _, blocked := range []string{"", "bob@partner.org", "eve@sub.allowed.io", "nobody@evil.com"} {
		if dFiltered.isAllowed(blocked) {
			t.Fatalf("expected %q to be blocked by allowlist", blocked)
		}
	}
}

func TestInboundParsingAllowlistThreadingAndAttachments(t *testing.T) {
	tmp := t.TempDir()
	store, err := media.Open(filepath.Join(tmp, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	pol := media.Policy{Allow: []string{"image/*", "text/plain"}, MaxBytes: 1 << 20}
	sink := &captureSink{}

	var dropped []string
	d := New("mail", "assistant", Options{
		IMAPHost:     "imap.example.com",
		SMTPHost:     "127.0.0.1",
		Username:     "bot@example.com",
		Password:     "secret",
		AllowFrom:    []string{"vip@partner.org"},
		AllowDomains: []string{"trusted.com"},
		MarkSeen:     true,
	}, sink, store, pol, nil)
	d.OnDrop = func(channel, agent, chatID, sender, text, reason string) {
		dropped = append(dropped, fmt.Sprintf("%s|%s|%s|%s|%s", channel, agent, chatID, sender, reason))
	}

	// 1. Blocked sender hits OnDrop and is not routed.
	blockedRaw := "From: Mallory <mallory@evil.com>\r\n" +
		"To: bot@example.com\r\n" +
		"Subject: Spam\r\n" +
		"Message-ID: <spam-1@evil.com>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nhello spam\r\n"
	if accepted, markSeen := d.processRawMessage(1, 10, []byte(blockedRaw)); accepted || markSeen {
		t.Fatalf("blocked email should not be accepted or marked seen: accepted=%v markSeen=%v", accepted, markSeen)
	}
	if len(dropped) != 1 || dropped[0] != "mail|assistant|thread:spam-1@evil.com|user:email:mallory@evil.com|allowlist" {
		t.Fatalf("dropped = %v", dropped)
	}

	// 2. Own address & Auto-Submitted messages are suppressed without OnDrop.
	selfRaw := "From: bot@example.com\r\nTo: alice@trusted.com\r\nMessage-ID: <self-1@example.com>\r\nSubject: Re: Hi\r\n\r\nhi\r\n"
	if accepted, _ := d.processRawMessage(1, 11, []byte(selfRaw)); accepted {
		t.Fatal("self email must be ignored")
	}
	autoRaw := "From: alice@trusted.com\r\nTo: bot@example.com\r\nAuto-Submitted: auto-replied\r\nMessage-ID: <auto-1@trusted.com>\r\nSubject: OOO\r\n\r\nout of office\r\n"
	if accepted, _ := d.processRawMessage(1, 12, []byte(autoRaw)); accepted {
		t.Fatal("auto-submitted message must be ignored")
	}
	if len(dropped) != 1 {
		t.Fatalf("expected no additional drops, got %v", dropped)
	}

	// 3. Multipart allowed message with References + HTML fallback + PNG attachment + disallowed exe.
	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1, 2, 3}
	multiRaw := "From: Alice <Alice@Trusted.com>\r\n" +
		"Reply-To: Alice Reply <reply@trusted.com>\r\n" +
		"To: bot@example.com\r\n" +
		"Cc: cc@trusted.com\r\n" +
		"Subject: Quarterly Report\r\n" +
		"Message-ID: <msg-2@trusted.com>\r\n" +
		"In-Reply-To: <msg-1@trusted.com>\r\n" +
		"References: <root-0@trusted.com> <msg-1@trusted.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"bnd123\"\r\n\r\n" +
		"--bnd123\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<div><p>Hello <b>Bot</b>!</p><p>See chart &amp; reply.</p><script>bad()</script></div>\r\n" +
		"--bnd123\r\n" +
		"Content-Type: image/png; name=\"chart.png\"\r\n" +
		"Content-Disposition: attachment; filename=\"chart.png\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(pngBytes) + "\r\n" +
		"--bnd123\r\n" +
		"Content-Type: application/x-msdownload; name=\"payload.exe\"\r\n" +
		"Content-Disposition: attachment; filename=\"payload.exe\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("MZ")) + "\r\n" +
		"--bnd123--\r\n"

	accepted, shouldMark := d.processRawMessage(1, 13, []byte(multiRaw))
	if !accepted || !shouldMark {
		t.Fatalf("valid allowed email should be accepted and markSeen: accepted=%v markSeen=%v", accepted, shouldMark)
	}
	inbs := sink.list()
	if len(inbs) != 1 {
		t.Fatalf("expected 1 routed message, got %d", len(inbs))
	}
	msg := inbs[0].Message
	if msg.From != "user:email:alice@trusted.com" {
		t.Fatalf("From = %q", msg.From)
	}
	if msg.ReplyTo != "thread:root-0@trusted.com" {
		t.Fatalf("ReplyTo = %q", msg.ReplyTo)
	}
	if !strings.Contains(msg.Text, "Subject: Quarterly Report") || !strings.Contains(msg.Text, "Hello Bot!") || !strings.Contains(msg.Text, "See chart & reply.") || strings.Contains(msg.Text, "bad()") {
		t.Fatalf("unexpected extracted body text: %q", msg.Text)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].Name != "chart.png" || msg.Attachments[0].MIME != "image/png" || msg.Attachments[0].Handle == "" {
		t.Fatalf("expected 1 PNG attachment (and .exe skipped), got %+v", msg.Attachments)
	}
	if msg.Payload["chat_id"] != "thread:root-0@trusted.com" || msg.Payload["name"] != "Alice" || msg.Payload["reply_to_email"] != "reply@trusted.com" {
		t.Fatalf("unexpected payload: %+v", msg.Payload)
	}
}

func TestIMAPAndSMTPWireRoundTripAndTestConnection(t *testing.T) {
	inboundMail := "From: Partner <partner@allowed.org>\r\n" +
		"To: bot@example.com\r\n" +
		"Subject: Need status update\r\n" +
		"Message-ID: <q-100@allowed.org>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nAny updates on the release?\r\n"

	imapAddr, closeIMAP := startMockIMAP(t, "bot@example.com", "secret123", inboundMail)
	defer closeIMAP()
	smtpSrv := startMockSMTP(t, "bot@example.com", "secret123")
	defer smtpSrv.close()

	// 1. TestConnection succeeds with expected metadata.
	info, err := TestConnection(context.Background(), TestOptions{
		IMAPHost: "127.0.0.1",
		IMAPPort: 993,
		SMTPHost: "127.0.0.1",
		SMTPPort: 465,
		Username: "bot@example.com",
		Password: "secret123",
		Mailbox:  "INBOX",
		DialIMAP: dialPlainIMAPForTest(imapAddr),
		DialSMTP: dialPlainSMTPForTest(smtpSrv.addr()),
	})
	if err != nil {
		t.Fatalf("TestConnection valid failed: %v", err)
	}
	if !info.IMAPOK || !info.SMTPOK || !info.IDLESupport || info.Messages != 1 || info.Mailbox != "INBOX" {
		t.Fatalf("unexpected ConnectionInfo: %+v", info)
	}

	// 2. TestConnection scrubs secret when auth fails.
	_, err = TestConnection(context.Background(), TestOptions{
		IMAPHost: "127.0.0.1",
		SMTPHost: "127.0.0.1",
		Username: "bot@example.com",
		Password: "wrong-password-supersecret",
		DialIMAP: dialPlainIMAPForTest(imapAddr),
		DialSMTP: dialPlainSMTPForTest(smtpSrv.addr()),
	})
	if err == nil || strings.Contains(err.Error(), "wrong-password-supersecret") {
		t.Fatalf("expected scrubbed auth error, got %v", err)
	}

	// 3. Driver fetches from mock IMAP, marks \Seen, and delivers threaded reply with attachment via mock SMTP.
	sink := &captureSink{}
	tmp := t.TempDir()
	store, err := media.Open(filepath.Join(tmp, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	d := New("mail", "helper", Options{
		IMAPHost:     "127.0.0.1",
		IMAPPort:     993,
		SMTPHost:     "127.0.0.1",
		SMTPPort:     465,
		Username:     "bot@example.com",
		Password:     "secret123",
		FromAddress:  "Helper Bot <bot@example.com>",
		Mailbox:      "INBOX",
		Mode:         "polling",
		PollInterval: 50 * time.Millisecond,
		MarkSeen:     true,
		AllowDomains: []string{"allowed.org"},
		DialIMAP:     dialPlainIMAPForTest(imapAddr),
		DialSMTP:     dialPlainSMTPForTest(smtpSrv.addr()),
	}, sink, store, media.Policy{Allow: []string{"text/plain"}, MaxBytes: 1 << 20}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(4 * time.Second)
	for len(sink.list()) == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	inbs := sink.list()
	if len(inbs) != 1 {
		t.Fatalf("expected 1 email delivered from mock IMAP, got %d", len(inbs))
	}
	// Verify server marked the message \Seen.
	ic, err := dialPlainIMAPForTest(imapAddr)(context.Background(), imapAddr, 993, &imapclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ic.Close()
	if err := ic.Login("bot@example.com", "secret123").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := ic.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	unseen, err := ic.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(unseen.AllUIDs()) != 0 {
		t.Fatalf("expected processed email to be marked \\Seen, remaining unseen UIDs: %v", unseen.AllUIDs())
	}

	// Reply back into thread with an inline base64 attachment.
	attData := base64.StdEncoding.EncodeToString([]byte("release-notes-v1"))
	err = d.Deliver(inbs[0].Message.ReplyTo, "Everything is green for launch.", []media.Part{
		{Type: "file", MIME: "text/plain", Name: "notes.txt", Data: attData},
	})
	if err != nil {
		t.Fatalf("Deliver failed: %v", err)
	}
	recs := smtpSrv.records()
	if len(recs) != 1 {
		t.Fatalf("expected 1 SMTP message sent, got %d", len(recs))
	}
	sent := recs[0]
	if !strings.Contains(sent.from, "bot@example.com") || len(sent.to) != 1 || !strings.Contains(sent.to[0], "partner@allowed.org") {
		t.Fatalf("unexpected envelope from=%q to=%v", sent.from, sent.to)
	}
	for _, want := range []string{
		"Subject: Re: Need status update",
		"In-Reply-To: <q-100@allowed.org>",
		"References: <q-100@allowed.org>",
		"Content-Type: multipart/mixed",
		"Everything is green for launch.",
		"filename=notes.txt",
	} {
		if !slices.Contains([]bool{strings.Contains(sent.data, want)}, true) {
			t.Fatalf("outbound SMTP message missing %q:\n%s", want, sent.data)
		}
	}
}
