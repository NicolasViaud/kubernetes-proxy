//go:build !linux

package main

import (
	"fmt"
	"net"
)

// getOriginalDst is not supported on non-Linux platforms.
// The sidecar is always deployed inside a Linux container; this stub exists
// only so the workspace compiles on macOS/Windows development machines.
func getOriginalDst(_ net.Conn) (string, error) {
	return "", fmt.Errorf("SO_ORIGINAL_DST is only available on Linux")
}
