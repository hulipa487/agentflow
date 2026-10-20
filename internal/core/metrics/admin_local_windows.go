//go:build windows

package metrics

import (
	"fmt"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

// ListenLocalAdmin opens a named pipe for the admin plane. The DACL grants
// access to SYSTEM, Administrators and the pipe owner, so a Studio process
// running as the same user can connect while other local users cannot. The
// bearer token is still required.
func ListenLocalAdmin(name string) (net.Listener, error) {
	if name == "" {
		return nil, fmt.Errorf("empty admin pipe name")
	}
	if !strings.HasPrefix(name, `\\.\pipe\`) {
		name = `\\.\pipe\` + name
	}
	l, err := winio.ListenPipe(name, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;OW)",
	})
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", name, err)
	}
	return l, nil
}
