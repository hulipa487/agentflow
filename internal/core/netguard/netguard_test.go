package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// TestCheckBlocked is the deny-list table. It is the primary test for the
// guard: Check is pure, so every range can be pinned without a socket.
func TestCheckBlocked(t *testing.T) {
	blocked := map[string]string{
		// IPv4
		"0.0.0.0":         "this-network",
		"10.0.0.1":        "private",
		"172.16.0.1":      "private",
		"192.168.1.1":     "private",
		"100.64.0.1":      "carrier-grade NAT",
		"127.0.0.1":       "loopback",
		"127.9.9.9":       "loopback",
		"169.254.169.254": "cloud metadata",
		"192.0.2.1":       "documentation",
		"198.18.0.1":      "benchmarking",
		"198.51.100.1":    "documentation",
		"203.0.113.1":     "documentation",
		"224.0.0.1":       "multicast",
		"240.0.0.1":       "reserved",
		"255.255.255.255": "reserved",

		// IPv6
		"::":           "unspecified",
		"::1":          "loopback",
		"fc00::1":      "unique-local",
		"fd12:3456::1": "unique-local",
		"fe80::1":      "link-local",
		"ff02::1":      "multicast",
		"100::1":       "discard-only",
		"2001:db8::1":  "documentation",

		// The embedding prefixes: each can carry a private IPv4 inside it, so a
		// check that only asked "is this a private v6 range" would miss them.
		"2002:0a00:0001::": "6to4 wrapping 10.0.0.1",
		"64:ff9b::a00:1":   "NAT64 wrapping 10.0.0.1",
		"::10.0.0.1":       "IPv4-compatible wrapping 10.0.0.1",

		// v4-mapped v6. Unmap must run first or every v4 rule below is skipped.
		"::ffff:127.0.0.1":       "mapped loopback",
		"::ffff:10.0.0.1":        "mapped private",
		"::ffff:169.254.169.254": "mapped link-local",

		// Zoned addresses. netip.Prefix.Contains refuses to match anything
		// carrying a zone, so without an explicit denial every prefix rule and
		// the IsGlobalUnicast backstop below are skipped and a ULA address
		// sails straight through.
		"fc00::1%en0":   "zoned unique-local",
		"fe80::1%eth0":  "zoned link-local",
		"2001:db8::1%x": "zoned documentation",
		"::1%lo":        "zoned loopback",
	}
	for raw, want := range blocked {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("bad test address %q: %v", raw, err)
		}
		err = Policy{}.Check(addr)
		if err == nil {
			t.Fatalf("%s must be refused (%s)", raw, want)
		}
		var b *BlockedError
		if !errors.As(err, &b) {
			t.Fatalf("%s: want *BlockedError, got %T", raw, err)
		}
		if b.Reason == "" {
			t.Fatalf("%s: refusal must carry a reason", raw)
		}
	}
}

// TestCheckAllowed pins the other direction. Over-blocking is the failure mode
// nobody notices, so the public addresses a deployment actually fetches are
// asserted reachable.
func TestCheckAllowed(t *testing.T) {
	allowed := []string{
		"1.1.1.1",
		"8.8.8.8",
		"93.184.216.34", // example.com
		"2606:4700:4700::1111",
		// Outside 2001::/23 — the IETF block ends before RIR space begins, so
		// this must not be caught by the Teredo/documentation entry.
		"2001:4860:4860::8888",
		"::ffff:8.8.8.8", // mapped public
	}
	for _, raw := range allowed {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("bad test address %q: %v", raw, err)
		}
		if err := (Policy{}).Check(addr); err != nil {
			t.Fatalf("%s must be allowed, got %v", raw, err)
		}
	}
}

// TestCheckAllowPrivateDisablesGuard: the documented escape hatch.
func TestCheckAllowPrivateDisablesGuard(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "fc00::1", "::1"} {
		addr := netip.MustParseAddr(raw)
		if err := (Policy{AllowPrivate: true}).Check(addr); err != nil {
			t.Fatalf("AllowPrivate must permit %s, got %v", raw, err)
		}
	}
}

// TestGuardSilentByDefault: AllowPrivate is the only way to disable the guard,
// so the zero Policy must guard. This is a documented invariant of the type.
func TestGuardSilentByDefault(t *testing.T) {
	if err := (Policy{}).Check(netip.MustParseAddr("127.0.0.1")); err == nil {
		t.Fatal("the zero Policy must guard")
	}
}

// TestDialControlParsing covers the string form the dialer actually hands over.
// DialControl never touches the RawConn, so nil is a valid argument and the
// whole parse path is testable without a socket.
func TestDialControlParsing(t *testing.T) {
	blocked := []string{
		"127.0.0.1:80",
		"10.0.0.1:443",
		"[::1]:80",
		"[fc00::1%en0]:80",
		"[fe80::1%eth0]:80",
		"169.254.169.254:80",
		// Unbracketed, which is how the resolver can emit a zoned 4-in-6
		// address. Rejected as unparseable, which fails closed.
		"10.0.0.1%eth0:80",
		// Not an address at all — a unix socket path, or garbage.
		"/tmp/sock",
		"garbage",
		"",
	}
	for _, addr := range blocked {
		if err := (Policy{}).DialControl("tcp", addr, nil); err == nil {
			t.Fatalf("DialControl must refuse %q", addr)
		}
	}

	allowed := []string{"8.8.8.8:53", "[2606:4700:4700::1111]:443"}
	for _, addr := range allowed {
		if err := (Policy{}).DialControl("tcp", addr, nil); err != nil {
			t.Fatalf("DialControl must allow %q, got %v", addr, err)
		}
	}
}

// TestDialControlAllowPrivate: the opt-out covers the dial path too, which is
// what lets a guarded client reach an httptest server.
func TestDialControlAllowPrivate(t *testing.T) {
	if err := (Policy{AllowPrivate: true}).DialControl("tcp", "127.0.0.1:80", nil); err != nil {
		t.Fatalf("AllowPrivate must permit a loopback dial, got %v", err)
	}
}

// TestTransportRefusesLoopback is the end-to-end proof, and the important one:
// it shows a *BlockedError survives http.Client.Do's wrapping in *url.Error,
// which is what IsBlocked's errors.As depends on. If the error type or the
// wrapping ever changes, this is what catches it.
func TestTransportRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the handler must never be reached")
	}))
	defer srv.Close()

	client := &http.Client{Transport: Policy{}.Transport()}
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("a loopback fetch must fail")
	}
	if !IsBlocked(err) {
		t.Fatalf("expected a guard refusal through the transport, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "refusing to connect") {
		t.Fatalf("err = %v", err)
	}
}

// TestTransportAllowsLoopbackUnderAllowPrivate: the same request succeeds when
// the guard is off, so the test above is failing for the guard's reason and not
// for some unrelated transport problem.
func TestTransportAllowsLoopbackUnderAllowPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := &http.Client{Transport: Policy{AllowPrivate: true}.Transport()}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unguarded fetch must succeed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// TestDialContextBlocksPostResolution is the rebinding case, made hermetic. The
// resolution step belongs to the stdlib and cannot be redirected from here, so
// what this proves is the outcome: handed the address a hostile name would
// resolve to, the guarded dialer refuses — with no DNS in the loop.
func TestDialContextBlocksPostResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://") // 127.0.0.1:port
	tr := Policy{}.Transport()
	_, err := tr.DialContext(context.Background(), "tcp", host)
	if err == nil {
		t.Fatal("dialing a resolved loopback address must be refused")
	}
	if !IsBlocked(err) {
		t.Fatalf("expected a guard refusal, got %T: %v", err, err)
	}
}

// TestInsecureTransportStillGuards: skipping certificate verification is a
// decision about who the peer claims to be, not about where the connection
// goes. The two must not be coupled.
func TestInsecureTransportStillGuards(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the handler must never be reached")
	}))
	defer srv.Close()

	client := &http.Client{Transport: Policy{}.InsecureTransport()}
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("an insecure request to loopback must still be refused")
	}
	if !IsBlocked(err) {
		t.Fatalf("expected an address refusal, not a TLS error: %T: %v", err, err)
	}
}

// TestInsecureTransportSkipsVerification: against a self-signed server the
// verifying transport fails and the insecure one succeeds, so the toggle does
// what it says.
func TestInsecureTransportSkipsVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	verified := &http.Client{Transport: Policy{AllowPrivate: true}.Transport()}
	if _, err := verified.Get(srv.URL); err == nil {
		t.Fatal("a self-signed certificate must fail verification")
	}

	insecure := &http.Client{Transport: Policy{AllowPrivate: true}.InsecureTransport()}
	resp, err := insecure.Get(srv.URL)
	if err != nil {
		t.Fatalf("the insecure transport must accept a self-signed certificate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// TestTransportDoesNotHonorProxyEnv: the guard clears Proxy deliberately,
// because behind an egress proxy the dialer would see the proxy's address and
// the target would never be checked.
func TestTransportDoesNotHonorProxyEnv(t *testing.T) {
	if p := (Policy{}).Transport().Proxy; p != nil {
		t.Fatal("a guarded transport must not use a proxy: the dialer would judge the proxy, not the target")
	}
	if p := (Policy{}).InsecureTransport().Proxy; p != nil {
		t.Fatal("the insecure transport must not use a proxy either")
	}
}

// TestTransportHasNoCustomTLSDialer: a custom DialTLSContext replaces the
// dialer entirely and would skip Control. The stdlib leaves these nil and Clone
// copies them, so a future change to DefaultTransport would silently unguard
// every request.
func TestTransportHasNoCustomTLSDialer(t *testing.T) {
	tr := Policy{}.Transport()
	if tr.DialTLS != nil || tr.DialTLSContext != nil {
		t.Fatal("the guarded transport must not carry a custom TLS dialer — it bypasses Control")
	}
	if tr.DialContext == nil {
		t.Fatal("the guarded transport must carry a dialer")
	}
}

// FuzzCheck: no input should panic, and a zoned address must always be refused
// — that is the property the zone denial exists to guarantee.
func FuzzCheck(f *testing.F) {
	for _, s := range []string{"127.0.0.1", "fc00::1%en0", "::ffff:10.0.0.1", "1.1.1.1", "::", "2001:db8::1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return
		}
		_ = Policy{}.Check(addr)
		if addr.Zone() != "" {
			if err := (Policy{}).Check(addr); err == nil {
				t.Fatalf("zoned address %q must be refused", s)
			}
		}
	})
}
