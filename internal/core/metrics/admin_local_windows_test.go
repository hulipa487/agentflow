//go:build windows

package metrics

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestListenLocalAdminPipe(t *testing.T) {
	short := fmt.Sprintf("agentflow-admin-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	full := `\\.\pipe\` + short
	l, err := ListenLocalAdmin(short)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})}
	go func() { _ = srv.Serve(l) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return winio.DialPipeContext(ctx, full)
	}}
	c := &http.Client{Transport: tr}
	resp, err := c.Get("http://pipe/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}
