//go:build !windows

package metrics

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ListenLocalAdmin opens a unix domain socket for the admin plane. The socket
// is owner-only (0600) and its directory is created 0700 when missing. A stale
// socket (one nothing answers on) is removed; a live one is refused.
func ListenLocalAdmin(path string) (net.Listener, error) {
	if path == "" {
		return nil, fmt.Errorf("empty admin socket path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("admin socket directory: %w", err)
	}
	if fi, err := os.Stat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("admin socket %s exists and is not a socket", path)
		}
		conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("admin socket %s is already in use", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale admin socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat admin socket: %w", err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("chmod admin socket: %w", err)
	}
	return &unlinkListener{Listener: l, path: path}, nil
}

// unlinkListener removes the socket file when the listener closes, so a
// restart does not leave a stale inode behind.
type unlinkListener struct {
	net.Listener
	path string
}

func (u *unlinkListener) Close() error {
	err := u.Listener.Close()
	_ = os.Remove(u.path)
	return err
}
