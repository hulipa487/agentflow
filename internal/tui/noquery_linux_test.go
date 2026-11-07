package tui

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestNoTerminalQueryAtStartup runs this test binary again with a fresh pty as
// its controlling terminal and stdin/stdout, which is exactly when Bubble
// Tea's package init would query the terminal (see package noquery). The pty
// never answers, as a terminal without OSC 11 support would not: without the
// guard the child writes the queries and sits in a five-second read.
func TestNoTerminalQueryAtStartup(t *testing.T) {
	if os.Getenv("AGENTFLOW_NOQUERY_CHILD") == "1" {
		fmt.Println("child-ready")
		return
	}
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer master.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestNoTerminalQueryAtStartup$", "-test.count=1")
	cmd.Env = append(os.Environ(), "AGENTFLOW_NOQUERY_CHILD=1", "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// A new session with the pty as controlling terminal makes the child the
	// foreground process group; termenv only queries from the foreground.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()

	out := readUntilExit(master, cmd, 10*time.Second)
	elapsed := time.Since(start)
	if bytes.Contains(out, []byte("\x1b]11;?")) || bytes.Contains(out, []byte("\x1b[6n")) {
		t.Fatalf("the binary queried the terminal at startup (Bubble Tea's init ran before package noquery):\n%q", out)
	}
	if !bytes.Contains(out, []byte("child-ready")) {
		t.Fatalf("child did not run:\n%q", out)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("startup took %v; a terminal query is waiting for an answer", elapsed)
	}
}

func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err == nil {
		err = unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0)
	}
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

// readUntilExit drains the pty master (a full pty buffer would block the
// child) until the child exits or the deadline passes.
func readUntilExit(master *os.File, cmd *exec.Cmd, limit time.Duration) []byte {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	var out bytes.Buffer
	buf := make([]byte, 4096)
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		_ = master.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _ := master.Read(buf)
		out.Write(buf[:n])
		select {
		case <-done:
			_ = master.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, _ := master.Read(buf)
			out.Write(buf[:n])
			return out.Bytes()
		default:
		}
	}
	_ = cmd.Process.Kill()
	<-done
	return out.Bytes()
}
