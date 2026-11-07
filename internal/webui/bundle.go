package webui

import (
	"encoding/base64"
	"io"
	"net/http"

	"agentflow/internal/core/bundle"
)

// handleUserBundle is the full-fidelity export: a zip with the profile, linked
// handles, memory rows, file bytes and credentials sealed to the caller's
// X25519 public key. The privacy export next to it is a different document
// with a different audience; this one is for moving a person between cells.
func (u *UI) handleUserBundle(w http.ResponseWriter, r *http.Request) {
	if u.deps.Bundle == nil {
		writeErr(w, http.StatusServiceUnavailable, "the migration bundle service is not wired (identity, memory and files are required)")
		return
	}
	id := r.PathValue("id")
	var publicKey []byte
	if raw := r.URL.Query().Get("public_key"); raw != "" {
		var err error
		publicKey, err = base64.StdEncoding.DecodeString(raw)
		if err != nil {
			publicKey, err = base64.RawURLEncoding.DecodeString(raw)
		}
		if err != nil || len(publicKey) != 32 {
			writeErr(w, http.StatusBadRequest, "public_key must be a base64 raw 32-byte X25519 public key")
			return
		}
	}
	b, err := u.deps.Bundle.Export(r.Context(), id, publicKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	doc, err := b.Zip()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="agentflow-user-`+id+`.zip"`)
	_, _ = w.Write(doc)
}

// handleUserImport takes a bundle zip and the X25519 private key its sealed
// credentials were addressed to. The key travels in a multipart field, never a
// query string, so it does not end up in access logs.
func (u *UI) handleUserImport(w http.ResponseWriter, r *http.Request) {
	if u.deps.Bundle == nil {
		writeErr(w, http.StatusServiceUnavailable, "the migration bundle service is not wired")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<20)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "expected multipart/form-data with a bundle file")
		return
	}
	file, _, err := r.FormFile("bundle")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bundle file is required")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	b, err := bundle.ReadZip(data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var privateKey []byte
	if raw := r.FormValue("private_key"); raw != "" {
		privateKey, err = base64.StdEncoding.DecodeString(raw)
		if err != nil {
			privateKey, err = base64.RawURLEncoding.DecodeString(raw)
		}
		if err != nil || len(privateKey) != 32 {
			writeErr(w, http.StatusBadRequest, "private_key must be a base64 raw 32-byte X25519 private key")
			return
		}
	}
	rep, err := u.deps.Bundle.Import(r.Context(), b, privateKey)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep})
}
