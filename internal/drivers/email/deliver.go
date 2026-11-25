package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	netmail "net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message/mail"

	"agentflow/internal/core/media"
)

// Deliver implements gateway.Driver. Targets prefixed with "thread:" resolve
// stored thread metadata to reply in-thread with In-Reply-To / References and
// "Re:" subjects; targets prefixed with "mailto:" or bare email addresses send
// a direct message.
func (d *Driver) Deliver(replyTo, text string, attachments []media.Part) error {
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		return nil
	}
	toHeader, toAddr, subject, inReplyTo, refs, threadID, err := d.resolveTarget(replyTo)
	if err != nil {
		return err
	}
	if d.smtpHost == "" || d.smtpUser == "" || d.smtpPass == "" {
		return errors.New("email: smtp configuration is incomplete")
	}

	var hdr mail.Header
	hdr.Set("From", d.fromHeader)
	hdr.Set("To", toHeader)
	hdr.SetSubject(subject)
	hdr.SetDate(time.Now())
	if err := hdr.GenerateMessageID(); err != nil {
		return fmt.Errorf("email: generate message-id: %w", err)
	}
	outMsgID, _ := hdr.MessageID()
	outMsgID = normalizeMsgID(outMsgID)
	if inReplyTo != "" {
		hdr.SetMsgIDList("In-Reply-To", []string{inReplyTo})
	}
	if len(refs) > 0 {
		hdr.SetMsgIDList("References", refs)
	}

	var buf bytes.Buffer
	mw, err := mail.CreateWriter(&buf, hdr)
	if err != nil {
		return fmt.Errorf("email: compose header: %w", err)
	}
	if strings.TrimSpace(text) != "" || len(attachments) == 0 {
		var ih mail.InlineHeader
		ih.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
		tw, err := mw.CreateSingleInline(ih)
		if err != nil {
			return fmt.Errorf("email: compose body: %w", err)
		}
		if _, err := io.WriteString(tw, text); err != nil {
			_ = tw.Close()
			return fmt.Errorf("email: write body: %w", err)
		}
		if err := tw.Close(); err != nil {
			return fmt.Errorf("email: close body: %w", err)
		}
	}

	for i, att := range attachments {
		data, name, mimeType, err := d.materializeAttachment(att, i)
		if err != nil {
			return err
		}
		var ah mail.AttachmentHeader
		ah.SetContentType(mimeType, nil)
		ah.SetFilename(name)
		aw, err := mw.CreateAttachment(ah)
		if err != nil {
			return fmt.Errorf("email: attach %q: %w", name, err)
		}
		if _, err := aw.Write(data); err != nil {
			_ = aw.Close()
			return fmt.Errorf("email: write attachment %q: %w", name, err)
		}
		if err := aw.Close(); err != nil {
			return fmt.Errorf("email: close attachment %q: %w", name, err)
		}
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("email: finalize message: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), smtpSendTimeout)
	defer cancel()
	smtpAddr := net.JoinHostPort(d.smtpHost, strconv.Itoa(d.smtpPort))
	tlsCfg := cloneTLSConfig(d.smtpHost, d.tlsConfig)
	dialSMTP := d.dialSMTP
	if dialSMTP == nil {
		dialSMTP = defaultDialSMTP
	}
	sc, err := dialSMTP(ctx, d.smtpHost, smtpAddr, d.smtpPort, tlsCfg)
	if err != nil {
		return scrubSecrets(fmt.Errorf("email SMTP dial %s: %w", smtpAddr, err), d.password, d.smtpPass)
	}
	defer sc.Close()

	if err := smtpAuthenticate(sc, d.smtpUser, d.smtpPass, d.smtpHost); err != nil {
		return scrubSecrets(fmt.Errorf("email SMTP auth: %w", err), d.password, d.smtpPass)
	}
	envelopeFrom := d.fromAddr
	if envelopeFrom == "" {
		envelopeFrom = d.smtpUser
	}
	if err := sc.Mail(envelopeFrom); err != nil {
		return fmt.Errorf("email SMTP MAIL FROM: %w", err)
	}
	if err := sc.Rcpt(toAddr); err != nil {
		return fmt.Errorf("email SMTP RCPT TO %s: %w", toAddr, err)
	}
	wc, err := sc.Data()
	if err != nil {
		return fmt.Errorf("email SMTP DATA: %w", err)
	}
	if _, err := wc.Write(buf.Bytes()); err != nil {
		_ = wc.Close()
		return fmt.Errorf("email SMTP write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("email SMTP commit body: %w", err)
	}
	_ = sc.Quit()

	if threadID != "" && outMsgID != "" {
		d.upsertThread(threadID, "", "", subject, outMsgID, nil)
	}
	return nil
}

func (d *Driver) resolveTarget(replyTo string) (toHeader, toAddr, subject, inReplyTo string, refs []string, threadID string, err error) {
	trimmed := strings.TrimSpace(replyTo)
	if trimmed == "" {
		return "", "", "", "", nil, "", errors.New("email: empty reply target")
	}
	if tid, ok := strings.CutPrefix(trimmed, "thread:"); ok && tid != "" {
		st, found := d.threadSnapshot(tid)
		if !found || st.toAddr == "" {
			return "", "", "", "", nil, "", fmt.Errorf("email: unknown or unaddressable thread %q", tid)
		}
		toAddr = st.toAddr
		toHeader = (&netmail.Address{Name: st.toName, Address: st.toAddr}).String()
		subject = ensureReSubject(st.subject)
		inReplyTo = st.lastMsgID
		refs = st.references
		return toHeader, toAddr, subject, inReplyTo, refs, tid, nil
	}
	if addrPart, ok := strings.CutPrefix(trimmed, "mailto:"); ok {
		trimmed = strings.TrimSpace(addrPart)
	}
	parsed, perr := netmail.ParseAddress(trimmed)
	if perr != nil || parsed.Address == "" {
		return "", "", "", "", nil, "", fmt.Errorf("email: invalid target %q", replyTo)
	}
	return parsed.String(), parsed.Address, "Message from AgentFlow", "", nil, "", nil
}

func ensureReSubject(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "Re: (no subject)"
	}
	if len(s) >= 3 && strings.EqualFold(s[:3], "re:") {
		return s
	}
	return "Re: " + s
}

func (d *Driver) materializeAttachment(att media.Part, idx int) ([]byte, string, string, error) {
	name := strings.TrimSpace(att.Name)
	mimeType := strings.TrimSpace(att.MIME)
	if name == "" {
		name = fmt.Sprintf("attachment-%d%s", idx+1, extForMIME(mimeType))
	}
	if d.media != nil {
		spec := media.TargetSpec{MaxBytes: maxOutboundMediaBytes, AllowURLFetch: true}
		ctx, cancel := context.WithTimeout(context.Background(), smtpSendTimeout)
		defer cancel()
		blob, err := d.media.MaterializeBytes(ctx, media.ScopeMaintenance, att, spec)
		if err != nil {
			return nil, "", "", fmt.Errorf("email: materialize attachment %q: %w", name, err)
		}
		if blob.Name != "" {
			name = blob.Name
		}
		if blob.MIME != "" {
			mimeType = blob.MIME
		}
		if mimeType == "" {
			mimeType = normalizeMIME("", name)
		}
		return blob.Bytes, name, mimeType, nil
	}
	switch {
	case att.Handle != "" && d.store != nil:
		b, err := d.store.ReadAll(att.Handle, maxOutboundMediaBytes)
		if err != nil {
			return nil, "", "", fmt.Errorf("email: read attachment handle %q: %w", att.Handle, err)
		}
		if mimeType == "" {
			mimeType = normalizeMIME("", name)
		}
		return b, name, mimeType, nil
	case att.Data != "":
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(att.Data))
		if err != nil {
			return nil, "", "", fmt.Errorf("email: decode attachment base64: %w", err)
		}
		if mimeType == "" {
			mimeType = normalizeMIME("", name)
		}
		return b, name, mimeType, nil
	default:
		return nil, "", "", fmt.Errorf("email: attachment %q has no resolvable content (need handle or data)", name)
	}
}
