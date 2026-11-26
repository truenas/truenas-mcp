//go:build unix

package main

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO at the key path must not hang setup: keyFileID only opens a
// regular file.
func TestKeyFileIDFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan int, 1)
	go func() { done <- keyFileID(p) }()
	select {
	case got := <-done:
		if got != 0 {
			t.Errorf("keyFileID(fifo) = %d, want 0", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("keyFileID blocked on a FIFO")
	}
}
