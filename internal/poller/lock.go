package poller

import (
	"log/slog"
	"os"
	"syscall"
	"time"
)

type turnLock struct {
	path   string
	log    *slog.Logger
	warned bool
}

const slowAcquire = time.Second

func (l *turnLock) unlocked(reason string, err error) func() {
	if !l.warned {
		l.log.Warn("turn lock unavailable; reviewing without it", "path", l.path, "reason", reason, "err", err)
	}
	l.warned = true
	return func() {}
}

func (l *turnLock) acquire() func() {
	if l == nil || l.path == "" {
		return func() {}
	}
	f, err := os.Open(l.path)
	if err != nil {
		return l.unlocked("cannot open", err)
	}
	start := time.Now()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return l.unlocked("flock failed", err)
	}
	if waited := time.Since(start); waited >= slowAcquire {
		l.log.Info("turn lock acquired", "path", l.path, "waited", waited.Round(time.Second))
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
