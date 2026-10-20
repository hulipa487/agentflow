//go:build !windows

package main

// watchStdinInterrupt is a no-op outside Windows: a POSIX terminal turns
// ctrl+c into SIGINT for the foreground process, including when no TUI is
// reading.
func watchStdinInterrupt(func()) {}
