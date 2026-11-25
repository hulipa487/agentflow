package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"agentflow/internal/core/studio"
	"agentflow/internal/drivers/email"
	"agentflow/internal/drivers/qqbot"
	"agentflow/internal/drivers/telegram"
)

var testEmailConnection = email.TestConnection

// channelResponse flattens the channel list and adds the outcome of the write.
type channelResponse struct {
	studio.ChannelList
	OK              bool   `json:"ok"`
	RestartRequired bool   `json:"restart_required"`
	SavedNote       string `json:"saved_note,omitempty"`
}

func (u *UI) channelsReady(w http.ResponseWriter) (*studio.Channels, bool) {
	if u.deps.Channels == nil {
		writeErr(w, http.StatusServiceUnavailable, "the studio channels API is not enabled on this instance")
		return nil, false
	}
	return u.deps.Channels, true
}

func channelsResult(list studio.ChannelList, restartRequired bool, note string) channelResponse {
	return channelResponse{ChannelList: list, OK: true, RestartRequired: restartRequired, SavedNote: note}
}

// handleStudioChannels lists the persisted channels. Secrets are never sent;
// Has* and the env/credential reference are.
func (u *UI) handleStudioChannels(w http.ResponseWriter, r *http.Request) {
	ch, ok := u.channelsReady(w)
	if !ok {
		return
	}
	list, err := ch.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	restartRequired := false
	for _, c := range list.Channels {
		if !c.Live {
			restartRequired = true
			break
		}
	}
	writeJSON(w, http.StatusOK, channelsResult(list, restartRequired, ""))
}

type channelRequest struct {
	Index   int                 `json:"index"`
	Channel studio.ChannelDraft `json:"channel"`
}

// handleStudioChannelValidate dry-runs a draft against the deployment. It
// returns every error rather than only the first so the form can show them.
func (u *UI) handleStudioChannelValidate(w http.ResponseWriter, r *http.Request) {
	ch, ok := u.channelsReady(w)
	if !ok {
		return
	}
	var req channelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	errs := ch.Validate(req.Index, req.Channel)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     len(errs) == 0,
		"errors": errs,
	})
}

// handleStudioChannelCreate appends a channel.
func (u *UI) handleStudioChannelCreate(w http.ResponseWriter, r *http.Request) {
	u.saveChannel(w, r, -1)
}

// handleStudioChannelPut replaces the channel at the index.
func (u *UI) handleStudioChannelPut(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || idx < 0 {
		writeErr(w, http.StatusBadRequest, "invalid channel index")
		return
	}
	u.saveChannel(w, r, idx)
}

func (u *UI) saveChannel(w http.ResponseWriter, r *http.Request, idx int) {
	ch, ok := u.channelsReady(w)
	if !ok {
		return
	}
	var req struct {
		Channel studio.ChannelDraft `json:"channel"`
		MTime   int64               `json:"mtime"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	list, outcome, err := ch.SaveLive(idx, req.Channel, req.MTime)
	if err != nil {
		if errors.Is(err, studio.ErrChannelConflict) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	note := outcome.Note
	if note == "" {
		note = "saved; restart agentflow to bind the new channel set"
		if idx < 0 {
			note = "saved; restart agentflow to bind the new channel"
		}
	}
	writeJSON(w, http.StatusOK, channelsResult(list, !outcome.Applied, note))
}

// handleStudioChannelDelete removes the channel at the index.
func (u *UI) handleStudioChannelDelete(w http.ResponseWriter, r *http.Request) {
	ch, ok := u.channelsReady(w)
	if !ok {
		return
	}
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || idx < 0 {
		writeErr(w, http.StatusBadRequest, "invalid channel index")
		return
	}
	mtime, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64)
	list, outcome, err := ch.DeleteLive(idx, mtime)
	if err != nil {
		if errors.Is(err, studio.ErrChannelConflict) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	note := outcome.Note
	if note == "" {
		note = "deleted; restart agentflow to drop the channel"
	}
	writeJSON(w, http.StatusOK, channelsResult(list, !outcome.Applied, note))
}

// handleStudioTelegramTest checks a candidate or stored Telegram bot token via
// getMe and returns bot identity and privacy-mode diagnostics without storing
// or echoing the secret token.
func (u *UI) handleStudioTelegramTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Index   *int   `json:"index,omitempty"`
		Token   string `json:"token,omitempty"`
		APIBase string `json:"api_base,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	rawTok := strings.TrimSpace(req.Token)
	apiBase := strings.TrimSpace(req.APIBase)
	if rawTok == "" && req.Index != nil && *req.Index >= 0 {
		ch, ok := u.channelsReady(w)
		if !ok {
			return
		}
		storedTok, storedBase, err := ch.RawToken(*req.Index)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		rawTok = strings.TrimSpace(storedTok)
		if apiBase == "" {
			apiBase = strings.TrimSpace(storedBase)
		}
	}
	if rawTok == "" {
		writeErr(w, http.StatusBadRequest, "enter a Telegram bot token first")
		return
	}
	tok := rawTok
	if u.deps.ResolveSecret != nil {
		resolved, ok := u.deps.ResolveSecret(r.Context(), rawTok)
		if !ok || strings.TrimSpace(resolved) == "" {
			writeErr(w, http.StatusBadRequest, "bot token credential could not be resolved")
			return
		}
		tok = strings.TrimSpace(resolved)
	}
	info, err := telegram.TestToken(r.Context(), tok, apiBase, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":  true,
		"bot": info,
	})
}

// handleStudioQQTest verifies QQ Bot AppID + AppSecret by exchanging for an
// access token and checking /gateway/bot without storing or echoing secrets.
func (u *UI) handleStudioQQTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Index   *int   `json:"index,omitempty"`
		AppID   string `json:"app_id,omitempty"`
		Secret  string `json:"secret,omitempty"`
		APIBase string `json:"api_base,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	appID := strings.TrimSpace(req.AppID)
	rawSecret := strings.TrimSpace(req.Secret)
	apiBase := strings.TrimSpace(req.APIBase)
	if (appID == "" || rawSecret == "") && req.Index != nil && *req.Index >= 0 {
		ch, ok := u.channelsReady(w)
		if !ok {
			return
		}
		storedAppID, storedSecret, storedBase, err := ch.RawQQCredentials(*req.Index)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if appID == "" {
			appID = strings.TrimSpace(storedAppID)
		}
		if rawSecret == "" {
			rawSecret = strings.TrimSpace(storedSecret)
		}
		if apiBase == "" {
			apiBase = strings.TrimSpace(storedBase)
		}
	}
	if appID == "" {
		writeErr(w, http.StatusBadRequest, "enter a QQ AppID first")
		return
	}
	if rawSecret == "" {
		writeErr(w, http.StatusBadRequest, "enter a QQ AppSecret first")
		return
	}
	secret := rawSecret
	if u.deps.ResolveSecret != nil {
		resolved, ok := u.deps.ResolveSecret(r.Context(), rawSecret)
		if !ok || strings.TrimSpace(resolved) == "" {
			writeErr(w, http.StatusBadRequest, "app secret credential could not be resolved")
			return
		}
		secret = strings.TrimSpace(resolved)
	}
	info, err := qqbot.TestCredentials(r.Context(), appID, secret, apiBase, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":  true,
		"bot": info,
	})
}

func scrubEmailSecret(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, sec := range secrets {
		if sec != "" {
			msg = strings.ReplaceAll(msg, sec, "[redacted]")
		}
	}
	return msg
}

// handleStudioEmailTest verifies IMAP login/mailbox select and SMTP
// authentication without altering mailbox flags or echoing stored secrets.
func (u *UI) handleStudioEmailTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Index             *int   `json:"index,omitempty"`
		IMAPHost          string `json:"imap_host,omitempty"`
		IMAPPort          int    `json:"imap_port,omitempty"`
		SMTPHost          string `json:"smtp_host,omitempty"`
		SMTPPort          int    `json:"smtp_port,omitempty"`
		Username          string `json:"username,omitempty"`
		Password          string `json:"password,omitempty"`
		SMTPUsername      string `json:"smtp_username,omitempty"`
		SMTPPassword      string `json:"smtp_password,omitempty"`
		ClearSMTPPassword bool   `json:"clear_smtp_password,omitempty"`
		Mailbox           string `json:"mailbox,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	imapHost := strings.TrimSpace(req.IMAPHost)
	imapPort := req.IMAPPort
	smtpHost := strings.TrimSpace(req.SMTPHost)
	smtpPort := req.SMTPPort
	username := strings.TrimSpace(req.Username)
	rawPass := strings.TrimSpace(req.Password)
	smtpUser := strings.TrimSpace(req.SMTPUsername)
	rawSMTPPass := strings.TrimSpace(req.SMTPPassword)
	mailbox := strings.TrimSpace(req.Mailbox)

	if req.Index != nil && *req.Index >= 0 {
		ch, ok := u.channelsReady(w)
		if !ok {
			return
		}
		stored, err := ch.RawEmailCredentials(*req.Index)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if imapHost == "" {
			imapHost = strings.TrimSpace(stored.IMAPHost)
		}
		if imapPort <= 0 {
			imapPort = stored.IMAPPort
		}
		if smtpHost == "" {
			smtpHost = strings.TrimSpace(stored.SMTPHost)
		}
		if smtpPort <= 0 {
			smtpPort = stored.SMTPPort
		}
		if username == "" {
			username = strings.TrimSpace(stored.Username)
		}
		if rawPass == "" {
			rawPass = strings.TrimSpace(stored.Password)
		}
		if smtpUser == "" {
			smtpUser = strings.TrimSpace(stored.SMTPUsername)
		}
		if rawSMTPPass == "" && !req.ClearSMTPPassword {
			rawSMTPPass = strings.TrimSpace(stored.SMTPPassword)
		}
		if mailbox == "" {
			mailbox = strings.TrimSpace(stored.Mailbox)
		}
	}

	if imapHost == "" {
		writeErr(w, http.StatusBadRequest, "enter an IMAP host first")
		return
	}
	if smtpHost == "" {
		writeErr(w, http.StatusBadRequest, "enter an SMTP host first")
		return
	}
	if username == "" {
		writeErr(w, http.StatusBadRequest, "enter a mailbox username first")
		return
	}
	if rawPass == "" {
		writeErr(w, http.StatusBadRequest, "enter an IMAP password first")
		return
	}

	password := rawPass
	if u.deps.ResolveSecret != nil {
		resolved, ok := u.deps.ResolveSecret(r.Context(), rawPass)
		if !ok || strings.TrimSpace(resolved) == "" {
			writeErr(w, http.StatusBadRequest, "email password credential could not be resolved")
			return
		}
		password = strings.TrimSpace(resolved)
	}
	smtpPassword := rawSMTPPass
	if rawSMTPPass != "" && u.deps.ResolveSecret != nil {
		resolved, ok := u.deps.ResolveSecret(r.Context(), rawSMTPPass)
		if !ok || strings.TrimSpace(resolved) == "" {
			writeErr(w, http.StatusBadRequest, "SMTP password credential could not be resolved")
			return
		}
		smtpPassword = strings.TrimSpace(resolved)
	}

	info, err := testEmailConnection(r.Context(), email.TestOptions{
		IMAPHost:     imapHost,
		IMAPPort:     imapPort,
		SMTPHost:     smtpHost,
		SMTPPort:     smtpPort,
		Username:     username,
		Password:     password,
		SMTPUsername: smtpUser,
		SMTPPassword: smtpPassword,
		Mailbox:      mailbox,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, scrubEmailSecret(err, password, smtpPassword, rawPass, rawSMTPPass))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"email": info,
	})
}
