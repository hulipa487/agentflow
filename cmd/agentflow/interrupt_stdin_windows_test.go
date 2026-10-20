//go:build windows

package main

import (
	"io"
	"testing"
	"time"
)

func TestWatchCtrlCStopsThenForces(t *testing.T) {
	r, w := io.Pipe()
	stopped := make(chan struct{}, 1)
	forced := make(chan struct{}, 1)
	go watchCtrlC(r, func() { stopped <- struct{}{} }, func() { forced <- struct{}{} })

	// Ordinary input must not stop the engine.
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
		t.Fatal("ordinary input stopped the engine")
	case <-time.After(50 * time.Millisecond):
	}

	// The first ctrl+c byte stops it.
	if _, err := w.Write([]byte{'x', 0x03, 'y'}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("ctrl+c did not stop the engine")
	}

	// A second one forces exit while shutdown is still running.
	if _, err := w.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-forced:
	case <-time.After(time.Second):
		t.Fatal("second ctrl+c did not force exit")
	}
	_ = w.Close()
}

func TestWatchCtrlCReturnsOnEOF(t *testing.T) {
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() {
		watchCtrlC(r, func() {}, func() {})
		close(done)
	}()
	_ = w.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not return on EOF")
	}
}
