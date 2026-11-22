package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"agentflow/internal/core/studio"
)

type pluginsResponse struct {
	studio.PluginsList
	OK bool `json:"ok"`
}

func (u *UI) pluginsReady(w http.ResponseWriter) (*studio.Plugins, bool) {
	if u.deps.Plugins == nil {
		writeErr(w, http.StatusServiceUnavailable, "the studio plugins API is not enabled on this instance")
		return nil, false
	}
	return u.deps.Plugins, true
}

func writePluginErr(w http.ResponseWriter, err error) {
	if errors.Is(err, studio.ErrPluginConflict) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeErr(w, http.StatusBadRequest, err.Error())
}

func (u *UI) handleStudioPlugins(w http.ResponseWriter, r *http.Request) {
	p, ok := u.pluginsReady(w)
	if !ok {
		return
	}
	list, err := p.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pluginsResponse{PluginsList: list, OK: true})
}

type pluginValidateRequest struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	File   string `json:"file,omitempty"`
	Source string `json:"source"`
}

func (u *UI) handleStudioPluginValidate(w http.ResponseWriter, r *http.Request) {
	p, ok := u.pluginsReady(w)
	if !ok {
		return
	}
	var req pluginValidateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	errs := p.Validate(req.Kind, req.Name, req.File, req.Source)
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(errs) == 0, "errors": errs})
}

type pluginSourceRequest struct {
	Source string `json:"source"`
	MTime  int64  `json:"mtime"`
}

func (u *UI) handleStudioBuiltinPut(w http.ResponseWriter, r *http.Request) {
	p, ok := u.pluginsReady(w)
	if !ok {
		return
	}
	var req pluginSourceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	list, err := p.SaveBuiltin(r.PathValue("name"), req.Source, req.MTime)
	if err != nil {
		writePluginErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pluginsResponse{PluginsList: list, OK: true})
}

func (u *UI) handleStudioBuiltinDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := u.pluginsReady(w)
	if !ok {
		return
	}
	mtime, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64)
	list, err := p.ResetBuiltin(r.PathValue("name"), mtime)
	if err != nil {
		writePluginErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pluginsResponse{PluginsList: list, OK: true})
}

type pluginPackageCreateRequest struct {
	Name   string `json:"name"`
	File   string `json:"file,omitempty"`
	Source string `json:"source,omitempty"`
}

func (u *UI) handleStudioPackageCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := u.pluginsReady(w)
	if !ok {
		return
	}
	var req pluginPackageCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	list, err := p.CreatePackage(req.Name, req.File, req.Source)
	if err != nil {
		writePluginErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pluginsResponse{PluginsList: list, OK: true})
}

func (u *UI) handleStudioPackageDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := u.pluginsReady(w)
	if !ok {
		return
	}
	list, err := p.DeletePackage(r.PathValue("name"))
	if err != nil {
		writePluginErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pluginsResponse{PluginsList: list, OK: true})
}

func (u *UI) handleStudioPackageFilePut(w http.ResponseWriter, r *http.Request) {
	p, ok := u.pluginsReady(w)
	if !ok {
		return
	}
	var req pluginSourceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	list, err := p.SavePackageFile(r.PathValue("name"), r.PathValue("file"), req.Source, req.MTime)
	if err != nil {
		writePluginErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pluginsResponse{PluginsList: list, OK: true})
}

func (u *UI) handleStudioPackageFileDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := u.pluginsReady(w)
	if !ok {
		return
	}
	mtime, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64)
	list, err := p.DeletePackageFile(r.PathValue("name"), r.PathValue("file"), mtime)
	if err != nil {
		writePluginErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pluginsResponse{PluginsList: list, OK: true})
}
