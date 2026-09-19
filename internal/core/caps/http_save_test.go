package caps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/core/netguard"
	"agentflow/internal/core/session"
)

// TestHTTPRequestSaveTo: http.request with save_to lands the body in the
// calling session's scratch and reports the saved record in the reply.
func TestHTTPRequestSaveTo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))
	defer srv.Close()

	fm := testFileManager(t)
	h := HTTPHandlers(discardLogger(), nil, netguard.Policy{AllowPrivate: true}, fm)
	op := session.Op{Type: "http.request", Method: "GET", URL: srv.URL, SaveTo: "payload.json", Owner: "sess:S"}
	resp, ok := h["http.request"](context.Background(), op)
	if !ok {
		t.Fatalf("http.request failed: %s", resp)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatal(err)
	}
	saved, _ := result["saved"].(map[string]any)
	if saved == nil || saved["name"] != "payload.json" || !strings.HasPrefix(saved["handle"].(string), "media:") {
		t.Fatalf("reply missing saved record: %s", resp)
	}

	// The scratch record is readable under the owning session.
	e, err := fm.ScratchGet(context.Background(), "sess:S", "payload.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := fm.ReadBlob(context.Background(), e.Handle, 1<<20)
	if err != nil || string(b) != `{"hello":"world"}` {
		t.Fatalf("scratch bytes %q err %v", b, err)
	}
}

// TestHTTPRequestSaveToDisabled: with the file store disabled at boot the
// response still returns and reports the save failure honestly.
func TestHTTPRequestSaveToDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	h := HTTPHandlers(discardLogger(), nil, netguard.Policy{AllowPrivate: true}, nil)
	op := session.Op{Type: "http.request", Method: "GET", URL: srv.URL, SaveTo: "x.bin", Owner: "sess:S"}
	resp, ok := h["http.request"](context.Background(), op)
	if !ok {
		t.Fatalf("http.request must still succeed: %s", resp)
	}
	if !strings.Contains(resp, `"save_error"`) || !strings.Contains(resp, "unavailable") {
		t.Fatalf("expected a save_error report, got %s", resp)
	}
}
