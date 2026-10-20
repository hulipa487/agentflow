//go:build windows

package main

import (
	"io"
	"os"
)

// watchStdinInterrupt stops the engine on a ctrl+c typed into a
// pseudo-terminal. Windows delivers ctrl+c as a console control event only to
// processes attached to a real console; mintty (Git Bash), ConPTY hosts and
// some IDE terminals instead write the raw 0x03 byte to stdin. The TUI reads
// stdin and therefore sees it; a no-TUI engine consumes no input and would
// otherwise ignore the key forever.
//
// Reading stdin is safe here because the engine has no other use for it. A
// real console is unaffected: its ctrl+c becomes SIGINT and never reaches the
// stdin stream. stop() also releases the signal handler, so a second ctrl+c
// from a real console terminates immediately; a second byte does the same for
// terminals that never generate the event.
func watchStdinInterrupt(stop func()) {
	// A file redirected into stdin is input, not a keyboard. Only terminals,
	// pipes and consoles can carry a typed ctrl+c.
	if st, err := os.Stdin.Stat(); err == nil && st.Mode().IsRegular() {
		return
	}
	go watchCtrlC(os.Stdin, stop, func() { os.Exit(130) })
}

func watchCtrlC(r io.Reader, stop, force func()) {
	buf := make([]byte, 64)
	seen := false
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			if b != 0x03 {
				continue
			}
			if seen {
				force()
				return
			}
			seen = true
			stop()
		}
		if err != nil {
			return
		}
	}
}
