package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"agentflow/internal/core/studio"
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

func channelsResult(list studio.ChannelList, note string) channelResponse {
	return channelResponse{ChannelList: list, OK: true, RestartRequired: true, SavedNote: note}
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
	writeJSON(w, http.StatusOK, channelsResult(list, ""))
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
	list, err := ch.Save(idx, req.Channel, req.MTime)
	if err != nil {
		if errors.Is(err, studio.ErrChannelConflict) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, channelsResult(list,
		"saved; restart agentflow to bind the new channel set"))
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
	list, err := ch.Delete(idx, mtime)
	if err != nil {
		if errors.Is(err, studio.ErrChannelConflict) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, channelsResult(list,
		"deleted; restart agentflow to drop the channel"))
}
