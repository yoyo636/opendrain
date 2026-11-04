//go:build !windows

package core

import (
	"context"
	"os"
	"syscall"
	"time"
)

// acquireTaskLock serialises concurrent fetches of the same (url, target)
// across processes. flock is released by the kernel when the owning process
// dies — which is exactly the failure mode OpenDrain is built around (an
// agent killed mid-download) — so a crashed worker can never leave a stale
// lockfile that wedges the task forever.
func acquireTaskLock(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
