// Package netguard refuses outbound connections to addresses that are not
// public, so an agent cannot use the engine's HTTP paths to reach the host's
// own network — the admin server on loopback, an internal service on RFC1918,
// or a cloud metadata endpoint on link-local.
//
// # Why the check lives in the dialer
//
// The guard is installed as net.Dialer.Control rather than checked when a URL
// is parsed, and that placement is the entire point. Control runs after DNS
// resolution and immediately before connect, so it sees the concrete address
// being dialed. A check at parse time is defeated by DNS rebinding: the name
// is judged once, then resolves to something else by the time the connection
// is made. It also means redirects need no separate handling — every hop opens
// its own connection through the same transport, so every hop is checked by
// construction, including a hop that points somewhere the original URL did
// not.
//
// # What this package does not do
//
// It does not log and it does not count. It returns a *BlockedError and the
// caller decides how to alert, which keeps this package free of dependencies
// on the logging and metrics layers.
//
// It also cannot help against an agent that has been given a shell: a
// container with `network: host` or an SSH handle reaches whatever that
// network reaches, and no dialer of ours is involved.
package netguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// Policy is an outbound-connection policy. The zero value guards; only
// AllowPrivate turns the guard off.
type Policy struct {
	// AllowPrivate disables the guard: every address is permitted. See
	// config.NetHTTP.AllowPrivate for why this exists at all.
	AllowPrivate bool
}

// BlockedError reports a refused connection. Callers detect it with
// errors.As (or IsBlocked) to count and log the refusal; the message names
// both the address and the rule that fired, so an operator reading a log line
// does not have to work out which range caught it.
type BlockedError struct {
	Addr   netip.Addr
	Reason string
}

func (e *BlockedError) Error() string {
	if !e.Addr.IsValid() {
		return "refusing to connect: " + e.Reason
	}
	return fmt.Sprintf("refusing to connect to %s: %s", e.Addr, e.Reason)
}

// IsBlocked reports whether err, or anything it wraps, is a guard refusal.
// The transport wraps dial errors in *url.Error, so a direct type assertion at
// the call site would miss it.
func IsBlocked(err error) bool {
	var b *BlockedError
	return errors.As(err, &b)
}

// blockedPrefixes is the IANA special-purpose address registry, written out
// explicitly rather than derived from netip's predicates.
//
// IsGlobalUnicast alone is not a sufficient rule: it returns true for
// carrier-grade NAT, the documentation ranges and 240/4, which are routable
// only inside a provider's own network and are exactly what an SSRF wants.
//
// The v6 entries to read twice are the ones that can carry an IPv4 address
// inside them — 2002::/16 (6to4) and 64:ff9b::/96 (NAT64). A check that only
// asked "is this a private v6 range" would let 2002:0a00:0001:: through, which
// is 6to4 wrapping 10.0.0.1.
var blockedPrefixes = []struct {
	prefix netip.Prefix
	reason string
}{
	// IPv4
	{netip.MustParsePrefix("0.0.0.0/8"), "this-network"},
	{netip.MustParsePrefix("10.0.0.0/8"), "private (RFC 1918)"},
	{netip.MustParsePrefix("100.64.0.0/10"), "carrier-grade NAT (RFC 6598)"},
	{netip.MustParsePrefix("127.0.0.0/8"), "loopback"},
	{netip.MustParsePrefix("169.254.0.0/16"), "link-local (cloud metadata endpoint)"},
	{netip.MustParsePrefix("172.16.0.0/12"), "private (RFC 1918)"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignments"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation (TEST-NET-1)"},
	{netip.MustParsePrefix("192.88.99.0/24"), "6to4 relay anycast"},
	{netip.MustParsePrefix("192.168.0.0/16"), "private (RFC 1918)"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking (RFC 2544)"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation (TEST-NET-2)"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation (TEST-NET-3)"},
	{netip.MustParsePrefix("192.31.196.0/24"), "AS112 anycast"},
	{netip.MustParsePrefix("192.52.193.0/24"), "AMT anycast"},
	{netip.MustParsePrefix("192.175.48.0/24"), "AS112 direct delegation"},
	{netip.MustParsePrefix("224.0.0.0/4"), "multicast"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved (includes broadcast)"},

	// IPv6
	{netip.MustParsePrefix("::/128"), "unspecified"},
	{netip.MustParsePrefix("::1/128"), "loopback"},
	// IPv4-compatible IPv6: the third prefix that can carry an IPv4 address,
	// after 6to4 and NAT64 below. Deprecated and not translated by modern
	// stacks, so the least dangerous of the three, but it is the same class and
	// costs one line. ::ffff:0:0/96 (v4-mapped) is deliberately not inside it
	// and is handled by Unmap in Check instead.
	{netip.MustParsePrefix("::/96"), "IPv4-compatible (embeds an IPv4 address)"},
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64 (embeds an IPv4 address)"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "NAT64 local-use (embeds an IPv4 address)"},
	{netip.MustParsePrefix("100::/64"), "discard-only"},
	{netip.MustParsePrefix("2001::/23"), "IETF protocol assignments (includes Teredo)"},
	// 2001:db8::/32 is NOT inside 2001::/23: a /23 fixes the second group to
	// 0x0000-0x01ff, and 0x0db8 is well outside that. It has to be listed
	// separately, and IsGlobalUnicast does not catch it either.
	{netip.MustParsePrefix("2001:db8::/32"), "documentation"},
	{netip.MustParsePrefix("2002::/16"), "6to4 (embeds an IPv4 address)"},
	{netip.MustParsePrefix("2620:4f:8000::/48"), "AS112 direct delegation"},
	{netip.MustParsePrefix("3fff::/20"), "documentation (RFC 9637)"},
	{netip.MustParsePrefix("5f00::/16"), "SRv6 SIDs"},
	{netip.MustParsePrefix("fc00::/7"), "unique-local (RFC 4193)"},
	{netip.MustParsePrefix("fe80::/10"), "link-local"},
	{netip.MustParsePrefix("ff00::/8"), "multicast"},
}

// Check reports whether an address may be connected to. It is pure, which is
// what makes the whole deny list testable without opening a socket.
//
// The order of the three steps below matters and is not cosmetic.
func (p Policy) Check(addr netip.Addr) error {
	if p.AllowPrivate {
		return nil
	}
	if !addr.IsValid() {
		return p.block(addr, "unparseable address")
	}
	// 1. A zone identifier must be refused before anything else, because it
	// defeats everything after it. netip.Prefix.Contains will not match an
	// address that carries a zone, so a zoned address falls through every
	// prefix rule below — and IsGlobalUnicast is true for ULA and the
	// documentation ranges, so the backstop does not catch it either. The
	// result is that "[fc00::1%en25]" reached a unique-local address with the
	// guard none the wiser. Unmap does not help: it drops a zone only from a
	// 4-in-6 address.
	//
	// Refusing outright is also the honest answer rather than stripping the
	// zone and carrying on: a zone names *which link* to reach the address on,
	// which is precisely the thing this guard exists to keep an agent off. No
	// HTTP path here legitimately dials one — net/http strips the zone from the
	// Host header for exactly this reason (RFC 6874).
	if addr.Zone() != "" {
		return p.block(addr, "scoped address names a local link")
	}
	// 2. Judge by family. Unmapping first is required: a v4-mapped v6 address
	// names the same host as its v4 form, and treating it as v6 would walk it
	// past every v4 rule.
	a := addr.Unmap()
	want4 := a.Is4()
	for _, b := range blockedPrefixes {
		if b.prefix.Addr().Is4() != want4 {
			continue
		}
		if b.prefix.Contains(a) {
			return p.block(a, b.reason)
		}
	}
	// 3. Belt and braces, not a catch-all. IsGlobalUnicast is true for RFC1918,
	// carrier-grade NAT, ULA and the documentation ranges — it excludes only
	// unspecified, loopback, multicast and link-local, all of which the list
	// above already names. So this does not cover a range the list has not been
	// taught; it is a second opinion on the categories that are already there,
	// and it is why the list itself has to be complete.
	if !a.IsGlobalUnicast() {
		return p.block(a, "not a global unicast address")
	}
	return nil
}

// block builds the refusal, keeping the constructor in one place.
//
// Note for anyone wiring a counter to this: the error a caller finally sees is
// not always the refusal. net.Dialer tries every address a name resolves to and
// returns the first error, so a name listing [unreachable-public, private]
// reports the timeout and the refusal goes uncounted. That is a gap in the
// count, not in the guard — the connection was still refused — and it only
// arises in arrangements where no connection succeeds anyway.
func (p Policy) block(addr netip.Addr, reason string) error {
	return &BlockedError{Addr: addr, Reason: reason}
}

// DialControl is installed as net.Dialer.Control. It is called after DNS
// resolution and immediately before connect, with the resolved "ip:port", so
// it judges the address actually being dialed rather than the name that was
// asked for.
//
// The syscall.RawConn is unused: by the time this runs the address is already
// resolved, so there is nothing left to inspect on the socket itself.
func (p Policy) DialControl(network, address string, _ syscall.RawConn) error {
	if p.AllowPrivate {
		return nil
	}
	// Control also fires for unix sockets, where "address" is a filesystem
	// path rather than an ip:port. No HTTP path here dials one, so refuse
	// rather than guess at what a path means.
	switch network {
	case "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6":
	default:
		return p.block(netip.Addr{}, fmt.Sprintf("unexpected network %q (address %q)", network, address))
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		// An address this package cannot parse is one it cannot vouch for.
		return p.block(netip.Addr{}, fmt.Sprintf("unparseable dial address %q on %s", address, network))
	}
	return p.Check(ap.Addr())
}

// Transport returns a guarded transport.
//
// It clones the stdlib default rather than building one from scratch, so the
// stdlib's dial and TLS timeouts, HTTP/2 support and compression settings are
// inherited instead of hand-rolled.
//
// Proxy is cleared deliberately. The default is ProxyFromEnvironment, and
// behind an egress proxy the dial connects to the *proxy* — so Control would
// judge the proxy's address every time and the target would never be checked,
// silently, on any host with HTTP_PROXY set. The cost is that these paths do
// not use an egress proxy. That is the trade this guard makes, and it is a
// documented one rather than an oversight.
//
// Build this once per client and keep it: Clone touches http.DefaultTransport
// (it latches that transport's HTTP/2 setup as a side effect), so this is a
// boot-time operation and must not be called per request. The clone
// deliberately does not inherit that setup — it builds its own, bound to the
// guarded dialer.
func (p Policy) Transport() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Unreachable with the stdlib. A bare transport still guards, which
		// beats panicking on a security control.
		return &http.Transport{DialContext: p.dialContext(), Proxy: nil}
	}
	clone := t.Clone()
	clone.Proxy = nil
	clone.DialContext = p.dialContext()
	// A custom TLS dialer replaces the dialer wholesale, so Control would never
	// run. The stdlib default leaves both nil, but Clone copies them, so a
	// future change to DefaultTransport would silently unguard every request.
	clone.DialTLS = nil
	clone.DialTLSContext = nil
	return clone
}

// InsecureTransport returns Transport with certificate verification disabled.
//
// The address guard still applies. Skipping the certificate check is a
// decision about who the peer claims to be; it is not a decision about where
// the connection goes, and the two must not be coupled.
func (p Policy) InsecureTransport() *http.Transport {
	t := p.Transport()
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	} else {
		t.TLSClientConfig = t.TLSClientConfig.Clone()
	}
	t.TLSClientConfig.InsecureSkipVerify = true
	return t
}

// dialContext builds the guarded dialer. The timeouts match the stdlib
// default transport's, which is what Transport clones from.
func (p Policy) dialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   p.DialControl,
	}
	return d.DialContext
}
