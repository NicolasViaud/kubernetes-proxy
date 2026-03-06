//go:build linux

package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// inNetNS executes fn inside the Linux network namespace at nsPath.
//
// It uses runtime.LockOSThread so that the namespace switch is confined to a
// single OS thread.  The original namespace is always restored before the
// thread is unlocked — following Go's documented contract for thread-local
// state.
//
// Why not use a sub-process / nsenter binary?  Because the CNI binary is
// copied directly onto the host node and runs as root; calling the host's own
// iptables binary from within the correct netns via Setns is simpler and
// has no dependency on nsenter being present in PATH.
func inNetNS(nsPath string, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Save current network namespace so we can restore it.
	origNS, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("open current netns: %w", err)
	}
	defer origNS.Close()

	// Open the target network namespace.
	targetNS, err := os.Open(nsPath)
	if err != nil {
		return fmt.Errorf("open target netns %s: %w", nsPath, err)
	}
	defer targetNS.Close()

	// Enter the pod's network namespace.
	if err := setns(targetNS.Fd()); err != nil {
		return fmt.Errorf("enter netns: %w", err)
	}
	// Always restore the original namespace before returning.
	// The defer order guarantees this runs before UnlockOSThread.
	defer setns(origNS.Fd()) //nolint:errcheck

	return fn()
}

// setns calls the Linux setns(2) syscall to switch the current thread's
// network namespace to the one identified by fd.
func setns(fd uintptr) error {
	_, _, errno := syscall.RawSyscall(
		sysSETNS,
		fd,
		syscall.CLONE_NEWNET,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
