//go:build windows

package core

import "context"

// The Go standard library has no advisory flock on Windows. An O_EXCL
// lockfile would survive a killed process and wedge the task forever, which
// is worse than the race it prevents, so this platform deliberately runs
// without cross-process serialisation. State writes are still atomic
// (tmp + fsync + rename), so a concurrent reader never sees a torn file.
// A future iteration can use LockFileEx once a Windows-only dep is acceptable.
func acquireTaskLock(_ context.Context, _ string) (func(), error) {
	return func() {}, nil
}
