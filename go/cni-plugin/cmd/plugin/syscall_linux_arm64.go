//go:build linux && arm64

package main

// sysSETNS is the Linux setns(2) syscall number for arm64.
const sysSETNS uintptr = 278
