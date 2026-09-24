package webui

import (
	"net/http"
	"strings"
	"testing"
)

// TestModelRoutesWithoutAManager: the model routes dereferenced the live manager
// without checking, so a console built without one — which any embedder or test
// that wires a single surface does — panicked on a nil pointer instead of
// answering. Each route must report the reason.
func TestModelRoutesWithoutAManager(t *testing.T) {
	ui := New(Deps{}) // no Models
	routes := []struct{ method, path string }{
		{http.MethodGet, "/admin/api/models"},
		{http.MethodPut, "/admin/api/models/m"},
		{http.MethodDelete, "/admin/api/models/m"},
		{http.MethodPost, "/admin/api/models/m/test"},
		{http.MethodPost, "/admin/api/models/persist"},
		{http.MethodPost, "/admin/api/models/revert"},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked with no models manager: %v", r)
				}
			}()
			rec := do(t, ui.API(), route.method, route.path, map[string]any{})
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("got %d (%s); want 503", rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}
