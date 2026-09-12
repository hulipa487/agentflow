package caps

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/core/credentials"
	"agentflow/internal/core/session"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

// maxMailBody caps how much of a fetched message body we return to the loop,
// so a runaway mailbox can't OOM a worker. Larger bodies are truncated.
const maxMailBody = 256 * 1024

// MailHandlers returns op handlers for mail.imap.fetch and mail.smtp.send.
//
// creds is the encrypted per-tenant credential store; when non-nil, the
// password for an IMAP/SMTP login is resolved from auth={service=...} at call
// time (the username is taken from op.MailUser). When nil, auth references fail
// with a clear error. Hosts, ports, and usernames come from the op itself — the
// loop names them, the secret comes from the store, so a password never
// crosses the Lua bridge.
func MailHandlers(log *slog.Logger, creds *credentials.Store) map[string]session.OpHandler {
	logger := log.With("module", "caps.mail")
	fail := func(err error) (string, bool) {
		b, _ := json.Marshal(err.Error())
		return string(b), false
	}
	okJSON := func(v any) (string, bool) {
		b, err := json.Marshal(v)
		if err != nil {
			return fail(err)
		}
		return string(b), true
	}

	// resolvePassword fetches the password from the cred store for the user in
	// ctx. Errors never include the secret itself.
	resolvePassword := func(ctx context.Context, auth *session.CredentialRef) (string, error) {
		if auth == nil || auth.Service == "" {
			return "", fmt.Errorf("mail: auth={service=...} is required")
		}
		if creds == nil {
			return "", fmt.Errorf("mail: auth=%q but credentials are not enabled", auth.Service)
		}
		uuid := session.UserUUIDFromCtx(ctx)
		if uuid == "" {
			return "", fmt.Errorf("mail: auth=%q but no user in context", auth.Service)
		}
		cred, ok, err := creds.Get(ctx, uuid, auth.Service)
		if err != nil {
			return "", fmt.Errorf("mail: resolve auth %q: %w", auth.Service, err)
		}
		if !ok {
			return "", fmt.Errorf("mail: no credential for service %q", auth.Service)
		}
		return cred.Value, nil
	}

	return map[string]session.OpHandler{
		"mail.imap.fetch": func(ctx context.Context, op session.Op) (string, bool) {
			host := strings.TrimSpace(op.MailHost)
			if host == "" {
				return fail(fmt.Errorf("mail.imap.fetch: host is required"))
			}
			port := op.MailPort
			if port == 0 {
				port = 993
			}
			user := strings.TrimSpace(op.MailUser)
			if user == "" {
				return fail(fmt.Errorf("mail.imap.fetch: username is required"))
			}
			password, err := resolvePassword(ctx, op.Auth)
			if err != nil {
				return fail(err)
			}
			mbox := strings.TrimSpace(op.Mailbox)
			if mbox == "" {
				mbox = "INBOX"
			}
			limit := op.Limit
			if limit <= 0 {
				limit = 10
			}

			addr := net.JoinHostPort(host, strconv.Itoa(port))
			dialer := &net.Dialer{Timeout: 15 * time.Second}
			conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
			if err != nil {
				return fail(fmt.Errorf("mail.imap.fetch: dial %s: %w", addr, err))
			}
			defer conn.Close()
			c := imapclient.New(conn, &imapclient.Options{TLSConfig: &tls.Config{ServerName: host}})
			defer c.Close()

			if err := c.Login(user, password).Wait(); err != nil {
				return fail(fmt.Errorf("mail.imap.fetch: login: %w", err))
			}

			m, err := c.Select(mbox, nil).Wait()
			if err != nil {
				return fail(fmt.Errorf("mail.imap.fetch: select %s: %w", mbox, err))
			}
			if m.NumMessages == 0 {
				return okJSON(map[string]any{"ok": true, "mailbox": mbox, "count": 0, "messages": []any{}})
			}

			// Build the set of sequence numbers to fetch.
			var seqs []uint32
			if op.Unseen {
				searchRes, err := c.Search(&imap.SearchCriteria{
					NotFlag: []imap.Flag{imap.FlagSeen},
				}, nil).Wait()
				if err != nil {
					return fail(fmt.Errorf("mail.imap.fetch: search: %w", err))
				}
				seqs = searchRes.AllSeqNums()
			} else {
				from := uint32(1)
				if m.NumMessages > uint32(limit) {
					from = m.NumMessages - uint32(limit) + 1
				}
				for n := from; n <= m.NumMessages; n++ {
					seqs = append(seqs, n)
				}
			}
			if len(seqs) > limit {
				seqs = seqs[len(seqs)-limit:]
			}
			if len(seqs) == 0 {
				return okJSON(map[string]any{"ok": true, "mailbox": mbox, "count": 0, "messages": []any{}})
			}

			seqSet := imap.SeqSet{}
			seqSet.AddNum(seqs...)
			fetchOpts := &imap.FetchOptions{
				Envelope: true,
				BodySection: []*imap.FetchItemBodySection{{Peek: true}},
			}
			messages, err := c.Fetch(&seqSet, fetchOpts).Collect()
			if err != nil {
				return fail(fmt.Errorf("mail.imap.fetch: fetch: %w", err))
			}

			out := make([]map[string]any, 0, len(messages))
			for _, msg := range messages {
				item := map[string]any{
					"seq":   msg.SeqNum,
					"uid":   msg.UID,
					"flags": msg.Flags,
				}
				if msg.Envelope != nil {
					item["subject"] = msg.Envelope.Subject
					if len(msg.Envelope.From) > 0 {
						a := msg.Envelope.From[0]
						item["from"] = a.Addr()
					}
					if len(msg.Envelope.To) > 0 {
						to := make([]string, 0, len(msg.Envelope.To))
						for _, a := range msg.Envelope.To {
							to = append(to, a.Addr())
						}
						item["to"] = to
					}
					if !msg.Envelope.Date.IsZero() {
						item["date"] = msg.Envelope.Date
					}
				}
				for _, bs := range msg.BodySection {
					if len(bs.Bytes) == 0 {
						continue
					}
					body := string(bs.Bytes)
					if len(body) > maxMailBody {
						body = body[:maxMailBody] + "\n…(truncated)"
					}
					item["body"] = body
					break
				}
				out = append(out, item)
			}
			logger.Debug("imap fetched", "host", addr, "mailbox", mbox, "count", len(out))
			return okJSON(map[string]any{"ok": true, "mailbox": mbox, "count": len(out), "messages": out})
		},

		"mail.smtp.send": func(ctx context.Context, op session.Op) (string, bool) {
			host := strings.TrimSpace(op.MailHost)
			if host == "" {
				return fail(fmt.Errorf("mail.smtp.send: host is required"))
			}
			port := op.MailPort
			if port == 0 {
				port = 465
			}
			user := strings.TrimSpace(op.MailUser)
			if user == "" {
				return fail(fmt.Errorf("mail.smtp.send: username is required"))
			}
			password, err := resolvePassword(ctx, op.Auth)
			if err != nil {
				return fail(err)
			}
			from := strings.TrimSpace(op.MailFrom)
			if from == "" {
				return fail(fmt.Errorf("mail.smtp.send: from is required"))
			}
			if len(op.MailTo) == 0 {
				return fail(fmt.Errorf("mail.smtp.send: at least one recipient is required"))
			}

			// Compose a text/plain RFC 822 message via go-message, which handles
			// MIME header encoding (subject) and transfer encoding correctly.
			hdr := mail.Header{}
			hdr.Set("From", from)
			hdr.Set("To", strings.Join(op.MailTo, ", "))
			hdr.SetSubject(op.Subject)
			hdr.SetDate(time.Now())
			hdr.Set("Content-Type", "text/plain; charset=utf-8")

			var raw strings.Builder
			mw, err := message.CreateWriter(&raw, hdr.Header)
			if err != nil {
				return fail(fmt.Errorf("mail.smtp.send: compose: %w", err))
			}
			if _, err := io.WriteString(mw, op.TextBody); err != nil {
				return fail(fmt.Errorf("mail.smtp.send: write body: %w", err))
			}
			if err := mw.Close(); err != nil {
				return fail(fmt.Errorf("mail.smtp.send: close body: %w", err))
			}

			addr := net.JoinHostPort(host, strconv.Itoa(port))
			// Port 465 = implicit TLS (SMTPS); 587/25 = STARTTLS.
			var client *smtp.Client
			if port == 465 {
				conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
				if err != nil {
					return fail(fmt.Errorf("mail.smtp.send: dial %s: %w", addr, err))
				}
				c, err := smtp.NewClient(conn, host)
				if err != nil {
					_ = conn.Close()
					return fail(fmt.Errorf("mail.smtp.send: smtp client: %w", err))
				}
				client = c
			} else {
				c, err := smtp.Dial(addr)
				if err != nil {
					return fail(fmt.Errorf("mail.smtp.send: dial %s: %w", addr, err))
				}
				if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
					_ = c.Close()
					return fail(fmt.Errorf("mail.smtp.send: starttls: %w", err))
				}
				client = c
			}
			defer client.Close()

			auth := smtp.PlainAuth("", user, password, host)
			if err := client.Auth(auth); err != nil {
				return fail(fmt.Errorf("mail.smtp.send: auth: %w", err))
			}
			if err := client.Mail(from); err != nil {
				return fail(fmt.Errorf("mail.smtp.send: MAIL FROM: %w", err))
			}
			for _, to := range op.MailTo {
				if err := client.Rcpt(to); err != nil {
					return fail(fmt.Errorf("mail.smtp.send: RCPT TO %s: %w", to, err))
				}
			}
			w, err := client.Data()
			if err != nil {
				return fail(fmt.Errorf("mail.smtp.send: DATA: %w", err))
			}
			if _, err := io.WriteString(w, raw.String()); err != nil {
				return fail(fmt.Errorf("mail.smtp.send: write body: %w", err))
			}
			if err := w.Close(); err != nil {
				return fail(fmt.Errorf("mail.smtp.send: close body: %w", err))
			}
			if err := client.Quit(); err != nil {
				logger.Debug("smtp quit error", "host", addr, "err", err)
			}
			logger.Debug("smtp sent", "host", addr, "from", from, "to", len(op.MailTo))
			return okJSON(map[string]any{"ok": true, "sent": len(op.MailTo)})
		},
	}
}
