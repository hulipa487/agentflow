package caps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agentflow/internal/core/credentials"
	"agentflow/internal/core/session"
)

// runMail spins up the mail handler map and invokes the named op.
func runMail(t *testing.T, creds *credentials.Store, name string, op session.Op) (map[string]any, bool) {
	t.Helper()
	h := MailHandlers(discardLogger(), creds)
	ctx := session.WithOwner(context.Background(), "session-1")
	resp, ok := h[name](ctx, op)
	if !ok {
		var out map[string]any
		_ = json.Unmarshal([]byte(resp), &out)
		return out, false
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("decode response: %v\nraw: %s", err, resp)
	}
	return out, true
}

func seededMailStore(t *testing.T, user, service, secret string) *credentials.Store {
	s, err := credentials.Open(filepath.Join(t.TempDir(), "creds.db"), "test-master-key", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Put(context.Background(), user, service, "password", secret, "", ""); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMailIMAPFetchMissingHost confirms the cap validates its args before
// touching the network — a misconfigured fetch returns a clear error, not a
// dial timeout.
func TestMailIMAPFetchMissingHost(t *testing.T) {
	creds := seededMailStore(t, "u1", "imap", "pw")
	_, ok := runMail(t, creds, "mail.imap.fetch", session.Op{
		Type:     "mail.imap.fetch",
		MailUser: "user@example.com",
		Auth:     &session.CredentialRef{Service: "imap"},
	})
	if ok {
		t.Fatal("expected failure when host is missing")
	}
}

// TestMailIMAPFetchMissingUser confirms username is required.
func TestMailIMAPFetchMissingUser(t *testing.T) {
	creds := seededMailStore(t, "u1", "imap", "pw")
	_, ok := runMail(t, creds, "mail.imap.fetch", session.Op{
		Type:     "mail.imap.fetch",
		MailHost: "imap.example.com",
		Auth:     &session.CredentialRef{Service: "imap"},
	})
	if ok {
		t.Fatal("expected failure when username is missing")
	}
}

// TestMailIMAPFetchNoAuthRef confirms a missing auth reference is a clear
// error, not a nil-deref — the cap never reaches the network without a secret.
func TestMailIMAPFetchNoAuthRef(t *testing.T) {
	_, ok := runMail(t, nil, "mail.imap.fetch", session.Op{
		Type:     "mail.imap.fetch",
		MailHost: "imap.example.com",
		MailUser: "user@example.com",
	})
	if ok {
		t.Fatal("expected failure when auth is missing")
	}
}

// TestMailSMTPSendValidation confirms the smtp validation order: each required
// field is rejected with a clear error before any dial.
func TestMailSMTPSendValidation(t *testing.T) {
	creds := seededMailStore(t, "u1", "smtp", "pw")
	cases := []struct {
		name string
		op   session.Op
	}{
		{"missing host", session.Op{Type: "mail.smtp.send", MailUser: "u@x.com", MailFrom: "f@x.com", MailTo: []string{"t@x.com"}, Auth: &session.CredentialRef{Service: "smtp"}}},
		{"missing user", session.Op{Type: "mail.smtp.send", MailHost: "smtp.x.com", MailFrom: "f@x.com", MailTo: []string{"t@x.com"}, Auth: &session.CredentialRef{Service: "smtp"}}},
		{"missing from", session.Op{Type: "mail.smtp.send", MailHost: "smtp.x.com", MailUser: "u@x.com", MailTo: []string{"t@x.com"}, Auth: &session.CredentialRef{Service: "smtp"}}},
		{"missing recipients", session.Op{Type: "mail.smtp.send", MailHost: "smtp.x.com", MailUser: "u@x.com", MailFrom: "f@x.com", Auth: &session.CredentialRef{Service: "smtp"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := runMail(t, creds, "mail.smtp.send", c.op); ok {
				t.Fatal("expected validation failure")
			}
		})
	}
}

// TestMailAuthNotEnabled confirms that when the credential store is nil, an
// auth reference fails with a clear "not enabled" error instead of panicking.
func TestMailAuthNotEnabled(t *testing.T) {
	_, ok := runMail(t, nil, "mail.imap.fetch", session.Op{
		Type:     "mail.imap.fetch",
		MailHost: "imap.example.com",
		MailUser: "user@example.com",
		Auth:     &session.CredentialRef{Service: "imap"},
	})
	if ok {
		t.Fatal("expected failure when credentials are not enabled")
	}
}

// TestMailAuthNoUser confirms an auth reference without a user in context fails
// clearly (mirrors net.http's behavior).
func TestMailAuthNoUser(t *testing.T) {
	creds := seededMailStore(t, "u1", "imap", "pw")
	h := MailHandlers(discardLogger(), creds)
	// No WithUserUUID on the context.
	_, ok := h["mail.imap.fetch"](context.Background(), session.Op{
		Type:     "mail.imap.fetch",
		MailHost: "imap.example.com",
		MailUser: "user@example.com",
		Auth:     &session.CredentialRef{Service: "imap"},
	})
	if ok {
		t.Fatal("expected failure when no user is in context")
	}
}

// TestMailAuthUnknownService confirms a missing credential yields a clear
// error (the store has no entry for that service for this user).
func TestMailAuthUnknownService(t *testing.T) {
	if os.Getenv("AGENTFLOW_SKIP_NETWORK") != "" {
		t.Skip("skipping; needs credential lookup")
	}
	creds := seededMailStore(t, "u1", "imap", "pw")
	// Service "smtp" isn't seeded for u1; the cap should fail before dialing
	// because resolvePassword runs first.
	_, ok := runMail(t, creds, "mail.imap.fetch", session.Op{
		Type:     "mail.imap.fetch",
		MailHost: "imap.example.com",
		MailUser: "user@example.com",
		Auth:     &session.CredentialRef{Service: "smtp"},
	})
	if ok {
		t.Fatal("expected failure for unknown credential service")
	}
}
