package email

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/router"
	"agentflow/internal/core/session"
)

const maxFetchBatch = 20

var (
	reScriptBlock = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyleBlock  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reBlockBreak  = regexp.MustCompile(`(?is)<\s*/?\s*(br|p|div|li|tr|h[1-6]|blockquote)\b[^>]*>`)
	reAnyTag      = regexp.MustCompile(`(?s)<[^>]+>`)
	reManyNewline = regexp.MustCompile(`\n{3,}`)
)

func (d *Driver) runLoop(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := d.runSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			d.log.Warn("email IMAP session ended", "err", scrubSecrets(err, d.password, d.smtpPass), "retry_in", backoff)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (d *Driver) runSession(ctx context.Context) error {
	notifyCh := make(chan struct{}, 4)
	addr := net.JoinHostPort(d.imapHost, strconv.Itoa(d.imapPort))
	tlsCfg := cloneTLSConfig(d.imapHost, d.tlsConfig)
	opts := &imapclient.Options{
		TLSConfig: tlsCfg,
		Dialer:    &net.Dialer{Timeout: connectTimeout},
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(_ *imapclient.UnilateralDataMailbox) {
				select {
				case notifyCh <- struct{}{}:
				default:
				}
			},
		},
	}
	dialIMAP := d.dialIMAP
	if dialIMAP == nil {
		dialIMAP = defaultDialIMAP
	}
	dCtx, dCancel := context.WithTimeout(ctx, connectTimeout)
	client, err := dialIMAP(dCtx, addr, d.imapPort, opts)
	dCancel()
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Login(d.username, d.password).Wait(); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	sel, err := client.Select(d.mailbox, nil).Wait()
	if err != nil {
		return fmt.Errorf("select %s: %w", d.mailbox, err)
	}

	canIdle := d.mode != "polling" && client.Caps().Has(imap.CapIdle)

	for {
		if err := d.syncMailbox(ctx, client, sel.UIDValidity); err != nil {
			return err
		}
		if ctx.Err() != nil {
			_ = client.Logout().Wait()
			return ctx.Err()
		}

		// Drain buffered unilateral updates accumulated during fetch.
		for draining := true; draining; {
			select {
			case <-notifyCh:
			default:
				draining = false
			}
		}

		if canIdle {
			idleWait := d.pollInterval
			if idleWait > maxIdleWait {
				idleWait = maxIdleWait
			}
			idleCmd, err := client.Idle()
			if err != nil {
				return fmt.Errorf("idle: %w", err)
			}
			idleDone := make(chan error, 1)
			go func() { idleDone <- idleCmd.Wait() }()

			timer := time.NewTimer(idleWait)
			var idleErr error
			select {
			case <-ctx.Done():
				timer.Stop()
				_ = idleCmd.Close()
				<-idleDone
				_ = client.Logout().Wait()
				return ctx.Err()
			case <-notifyCh:
				timer.Stop()
				_ = idleCmd.Close()
				idleErr = <-idleDone
			case <-timer.C:
				_ = idleCmd.Close()
				idleErr = <-idleDone
			case idleErr = <-idleDone:
				timer.Stop()
			}
			if idleErr != nil {
				return fmt.Errorf("idle wait: %w", idleErr)
			}
		} else {
			timer := time.NewTimer(d.pollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				_ = client.Logout().Wait()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

func (d *Driver) syncMailbox(ctx context.Context, client *imapclient.Client, uidValidity uint32) error {
	searchData, err := client.UIDSearch(&imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}, nil).Wait()
	if err != nil {
		return fmt.Errorf("uid search: %w", err)
	}
	allUIDs := searchData.AllUIDs()
	if len(allUIDs) == 0 {
		return nil
	}

	var pending []imap.UID
	for _, uid := range allUIDs {
		if !d.hasSeenUID(uidValidity, uid) {
			pending = append(pending, uid)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	for start := 0; start < len(pending); start += maxFetchBatch {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := start + maxFetchBatch
		if end > len(pending) {
			end = len(pending)
		}
		seqSet := imap.UIDSetNum(pending[start:end]...)
		fetchOpts := &imap.FetchOptions{
			UID:         true,
			Flags:       true,
			BodySection: []*imap.FetchItemBodySection{{Peek: true}},
		}
		msgs, err := client.Fetch(seqSet, fetchOpts).Collect()
		if err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
		for _, m := range msgs {
			d.handleFetchedMessage(client, uidValidity, m)
		}
	}
	return nil
}

func (d *Driver) handleFetchedMessage(client *imapclient.Client, uidValidity uint32, m *imapclient.FetchMessageBuffer) {
	if m == nil || m.UID == 0 {
		return
	}
	var raw []byte
	for _, bs := range m.BodySection {
		if len(bs.Bytes) > 0 {
			raw = bs.Bytes
			break
		}
	}
	if len(raw) == 0 {
		d.markSeenUID(uidValidity, m.UID)
		return
	}
	accepted, shouldMarkSeen := d.processRawMessage(uidValidity, m.UID, raw)
	d.markSeenUID(uidValidity, m.UID)
	if accepted && shouldMarkSeen && d.markSeen {
		uidSet := imap.UIDSetNum(m.UID)
		_, _ = client.Store(uidSet, &imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Silent: true,
			Flags:  []imap.Flag{imap.FlagSeen},
		}, nil).Collect()
	}
}

func (d *Driver) processRawMessage(uidValidity uint32, uid imap.UID, raw []byte) (accepted bool, shouldMarkSeen bool) {
	if int64(len(raw)) > maxRawEmailBytes {
		d.log.Warn("email skipped: message exceeds max size", "uid", uid, "bytes", len(raw))
		return false, false
	}

	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		d.log.Warn("email skipped: unreadable RFC5322 message", "uid", uid, "err", err)
		return false, false
	}
	defer mr.Close()

	// Ignore automated machine replies / vacation responders to prevent ping-pong loops.
	autoSub := strings.ToLower(strings.TrimSpace(mr.Header.Get("Auto-Submitted")))
	if autoSub != "" && autoSub != "no" {
		return false, false
	}

	fromList, _ := mr.Header.AddressList("From")
	if len(fromList) == 0 || strings.TrimSpace(fromList[0].Address) == "" {
		return false, false
	}
	fromAddr := strings.ToLower(strings.TrimSpace(fromList[0].Address))
	fromName := strings.TrimSpace(fromList[0].Name)
	if d.ownAddrs[fromAddr] {
		return false, false
	}

	replyToAddr, replyToName := fromAddr, fromName
	if rtList, _ := mr.Header.AddressList("Reply-To"); len(rtList) > 0 && strings.TrimSpace(rtList[0].Address) != "" {
		replyToAddr = strings.ToLower(strings.TrimSpace(rtList[0].Address))
		if n := strings.TrimSpace(rtList[0].Name); n != "" {
			replyToName = n
		}
	}

	subject, _ := mr.Header.Subject()
	subject = strings.TrimSpace(subject)
	msgID, _ := mr.Header.MessageID()
	msgID = normalizeMsgID(msgID)
	inReplyTo, _ := mr.Header.MsgIDList("In-Reply-To")
	refs, _ := mr.Header.MsgIDList("References")

	threadID := computeThreadID(uidValidity, uid, msgID, inReplyTo, refs)
	chatID := "thread:" + threadID
	sender := "user:email:" + fromAddr

	if !d.isAllowed(fromAddr) {
		d.log.Warn("email dropped by allowlist", "from", fromAddr, "uid", uid)
		if d.OnDrop != nil {
			d.OnDrop(d.name, d.agent, chatID, sender, subject, "allowlist")
		}
		return false, false
	}

	toAddrs := addressStrings(mr.Header, "To")
	ccAddrs := addressStrings(mr.Header, "Cc")
	msgDate, _ := mr.Header.Date()
	ts := time.Now().Unix()
	if !msgDate.IsZero() {
		ts = msgDate.Unix()
	}

	var plainParts []string
	var htmlParts []string
	var atts []media.Part

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			d.log.Warn("email part read error", "uid", uid, "err", err)
			break
		}
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			ct = strings.ToLower(strings.TrimSpace(ct))
			if ct == "" {
				ct = "text/plain"
			}
			switch ct {
			case "text/plain":
				b, _ := io.ReadAll(io.LimitReader(p.Body, maxBodyBytes))
				if txt := strings.TrimSpace(string(b)); txt != "" {
					plainParts = append(plainParts, txt)
				}
			case "text/html":
				b, _ := io.ReadAll(io.LimitReader(p.Body, maxBodyBytes))
				if txt := htmlToText(string(b)); txt != "" {
					htmlParts = append(htmlParts, txt)
				}
			default:
				if !strings.HasPrefix(ct, "multipart/") {
					_, dispParams, _ := h.ContentDisposition()
					fname := strings.TrimSpace(dispParams["filename"])
					if part, ok := d.ingestAttachment(fname, ct, p.Body); ok {
						atts = append(atts, part)
					}
				}
			}
		case *mail.AttachmentHeader:
			ct, _, _ := h.ContentType()
			fname, _ := h.Filename()
			if part, ok := d.ingestAttachment(fname, ct, p.Body); ok {
				atts = append(atts, part)
			}
		}
	}

	bodyText := strings.Join(plainParts, "\n\n")
	if strings.TrimSpace(bodyText) == "" {
		bodyText = strings.Join(htmlParts, "\n\n")
	}
	bodyText = strings.TrimSpace(bodyText)
	if bodyText == "" && subject == "" && len(atts) == 0 {
		return false, false
	}

	fullText := bodyText
	if subject != "" {
		if fullText != "" {
			fullText = "Subject: " + subject + "\n\n" + fullText
		} else {
			fullText = "Subject: " + subject
		}
	}

	var allRefs []string
	allRefs = append(allRefs, refs...)
	allRefs = append(allRefs, inReplyTo...)
	d.upsertThread(threadID, replyToAddr, replyToName, subject, msgID, allRefs)

	idToken := msgID
	if idToken == "" {
		idToken = fmt.Sprintf("%d-%d", uidValidity, uid)
	}
	payload := map[string]any{
		"chat_id":       chatID,
		"thread_id":     threadID,
		"mailbox":       d.mailbox,
		"uid":           uint32(uid),
		"uid_validity":  uidValidity,
		"from_email":    fromAddr,
		"reply_to_email": replyToAddr,
	}
	if fromName != "" {
		payload["name"] = fromName
	}
	if subject != "" {
		payload["subject"] = subject
	}
	if msgID != "" {
		payload["message_id"] = msgID
	}
	if len(inReplyTo) > 0 {
		payload["in_reply_to"] = inReplyTo
	}
	if len(refs) > 0 {
		payload["references"] = refs
	}
	if len(toAddrs) > 0 {
		payload["to"] = toAddrs
	}
	if len(ccAddrs) > 0 {
		payload["cc"] = ccAddrs
	}

	d.log.Debug("email received", "from", fromAddr, "thread_id", threadID, "uid", uid, "attachments", len(atts))
	d.sink.Submit(router.Inbound{
		Channel: d.name,
		Agent:   d.agent,
		Message: session.Message{
			ID:          "email-" + sanitizeMsgID(idToken),
			Type:        "user",
			From:        sender,
			Text:        fullText,
			Channel:     d.name,
			ReplyTo:     chatID,
			Attachments: atts,
			Ts:          ts,
			Payload:     payload,
		},
	})
	return true, true
}

func computeThreadID(uidValidity uint32, uid imap.UID, msgID string, inReplyTo, refs []string) string {
	for _, r := range refs {
		if norm := normalizeMsgID(r); norm != "" {
			return norm
		}
	}
	for _, r := range inReplyTo {
		if norm := normalizeMsgID(r); norm != "" {
			return norm
		}
	}
	if norm := normalizeMsgID(msgID); norm != "" {
		return norm
	}
	return fmt.Sprintf("uid:%d:%d", uidValidity, uid)
}

func addressStrings(h mail.Header, key string) []string {
	list, err := h.AddressList(key)
	if err != nil || len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		if addr := strings.ToLower(strings.TrimSpace(a.Address)); addr != "" {
			out = append(out, addr)
		}
	}
	return out
}

func sanitizeMsgID(id string) string {
	var b strings.Builder
	b.Grow(len(id))
	for i := 0; i < len(id) && i < 120; i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' || c == '@' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "msg"
	}
	return b.String()
}

func (d *Driver) ingestAttachment(filename, ct string, r io.Reader) (media.Part, bool) {
	if d.store == nil || !d.pol.Enabled() {
		return media.Part{}, false
	}
	name := strings.TrimSpace(filename)
	mimeType := normalizeMIME(ct, name)
	if !d.pol.Allows(mimeType) {
		metrics.Inc("agentflow_media_unsupported")
		d.log.Debug("email attachment skipped by media policy", "name", name, "mime", mimeType)
		return media.Part{}, false
	}
	limit := d.pol.MaxBytes
	if limit <= 0 {
		limit = defaultMaxMediaBytes
	}
	ref, err := d.store.Put(io.LimitReader(r, limit+1), mimeType, d.pol)
	if err != nil {
		metrics.Inc("agentflow_media_unsupported")
		d.log.Warn("email attachment store failed", "name", name, "mime", mimeType, "err", err)
		return media.Part{}, false
	}
	metrics.Inc("agentflow_media_ingested")
	metrics.Add("agentflow_media_bytes", ref.Size)
	if name == "" {
		name = "attachment" + extForMIME(mimeType)
	}
	return media.Part{
		Type:   media.Classify(mimeType),
		MIME:   mimeType,
		Handle: ref.Handle,
		Name:   name,
	}, true
}

func normalizeMIME(rawCT, filename string) string {
	base := strings.ToLower(strings.TrimSpace(strings.Split(rawCT, ";")[0]))
	if base != "" && base != "application/octet-stream" && strings.Contains(base, "/") {
		return base
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	case ".txt", ".log", ".md":
		return "text/plain"
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg", ".opus":
		return "audio/ogg"
	case ".mp4":
		return "video/mp4"
	case ".zip":
		return "application/zip"
	}
	if base != "" && strings.Contains(base, "/") {
		return base
	}
	return "application/octet-stream"
}

func extForMIME(mimeType string) string {
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "application/json":
		return ".json"
	default:
		return ".bin"
	}
}

func htmlToText(raw string) string {
	s := reScriptBlock.ReplaceAllString(raw, " ")
	s = reStyleBlock.ReplaceAllString(s, " ")
	s = reBlockBreak.ReplaceAllString(s, "\n")
	s = reAnyTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	s = reManyNewline.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
