package poller

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestTurnLockSerialisesHolders(t *testing.T) {
	dir := t.TempDir()
	lock := &turnLock{path: dir, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	release := lock.acquire()
	held := make(chan struct{})
	go func() {
		second := lock.acquire()
		close(held)
		second()
	}()
	select {
	case <-held:
		t.Fatal("second holder acquired the lock while the first held it")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("second holder never acquired the lock after release")
	}
}

func TestTurnLockRunsWithoutItWhenUnsetOrMissing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	(&turnLock{log: logger}).acquire()()
	missing := &turnLock{path: filepath.Join(t.TempDir(), "absent"), log: logger}
	missing.acquire()()
	if !missing.warned {
		t.Fatal("a missing path should warn")
	}
	var nilLock *turnLock
	nilLock.acquire()()
}
