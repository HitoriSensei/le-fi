package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// silenceStderrKeepingCopy redirects the process's real stderr (fd 2) to
// /dev/null and returns a duplicate file descriptor still connected to the
// original terminal, so our own status messages stay visible while
// cloudflared's own (always-on) console logging is suppressed.
//
// Uses golang.org/x/sys/unix rather than the standard syscall package:
// syscall.Dup2 is undefined on linux/arm64 (that syscall table only has
// dup3, not dup2 - a real build break found while cross-compiling this
// tool). unix.Dup2 abstracts over that difference on all our targets
// (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64).
func silenceStderrKeepingCopy() (ui *os.File, restore func()) {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return os.Stderr, func() {}
	}
	origFd, err := unix.Dup(int(os.Stderr.Fd()))
	if err != nil {
		devNull.Close()
		return os.Stderr, func() {}
	}
	if err := unix.Dup2(int(devNull.Fd()), int(os.Stderr.Fd())); err != nil {
		devNull.Close()
		return os.Stderr, func() {}
	}
	return os.NewFile(uintptr(origFd), "stderr-copy"), func() {
		_ = unix.Dup2(origFd, int(os.Stderr.Fd()))
		devNull.Close()
	}
}
