package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"agentflow/internal/core/studio"
	"agentflow/internal/drivers/telegram"
)

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
