package httpd

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// freePort returns an addr the OS guarantees is free for the test's lifetime.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestHealthEndpoint(t *testing.T) {
	s := New(freePort(t), nil)
	// health was registered in New; a GET should return 200 ok=true.
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != 200 {
		t.Fatalf("health status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("health body: %q", body)
	}
}

func TestHealthRejectsNonGet(t *testing.T) {
	s := New(freePort(t), nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodPost, "/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

func TestHandleDuplicatePathPanics(t *testing.T) {
	s := New(freePort(t), nil)
	s.Handle("/x/", func(http.ResponseWriter, *http.Request) {})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate path")
		}
	}()
	s.Handle("/x/", func(http.ResponseWriter, *http.Request) {})
}

func TestSubtreeRouting(t *testing.T) {
	s := New(freePort(t), nil)
	got := ""
	s.Handle("/webhook/dev/", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(200)
	})
	// a subtree path (trailing /) should match any sub-path under it
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook/dev/abc-123/", nil))
	if rec.Code != 200 || got != "/webhook/dev/abc-123/" {
		t.Fatalf("code=%d path=%q", rec.Code, got)
	}
	// and a sibling prefix must not match
	rec2 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/webhook/telegram/abc/", nil))
	if rec2.Code != 404 {
		t.Fatalf("unregistered path should 404, got %d", rec2.Code)
	}
}

func TestStartStopRoundTrip(t *testing.T) {
	addr := freePort(t)
	s := New(addr, nil)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	// health should respond on the real listener
	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health: %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.Stop(ctx)
}

func TestStartBindFailure(t *testing.T) {
	// take the port first so Start can't bind it
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s := New(ln.Addr().String(), nil)
	if err := s.Start(); err == nil {
		t.Fatal("expected bind error on taken port")
	}
}

func TestProbeEmptyURL(t *testing.T) {
	r := Probe(context.Background(), "")
	if r.OK || r.Err == nil {
		t.Fatalf("empty url should be OK=false with err, got %+v", r)
	}
}

func TestProbeHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	r := Probe(context.Background(), srv.URL)
	if !r.OK {
		t.Fatalf("expected OK, got err=%v", r.Err)
	}
}

func TestProbeUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", 503)
	}))
	defer srv.Close()
	r := Probe(context.Background(), srv.URL)
	if r.OK {
		t.Fatal("expected OK=false on 503")
	}
}

func TestProbeUnreachable(t *testing.T) {
	// a port that refuses connections: take then immediately drop
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	r := Probe(context.Background(), "http://"+addr)
	if r.OK {
		t.Fatalf("expected OK=false on refused connection")
	}
}