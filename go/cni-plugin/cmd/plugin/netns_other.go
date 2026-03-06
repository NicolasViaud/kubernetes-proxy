//go:build !linux

package main

import "fmt"

// inNetNS is a stub for non-Linux platforms.
// The CNI plugin binary is always deployed inside a Linux container on the
// host node; this file exists only so the Go workspace compiles on
// macOS/Windows developer machines.
func inNetNS(_ string, _ func() error) error {
	return fmt.Errorf("network namespace operations are only supported on Linux")
}
