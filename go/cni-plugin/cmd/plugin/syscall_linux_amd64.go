//go:build linux && amd64

package main

// sysSETNS is the Linux setns(2) syscall number for amd64.
const sysSETNS uintptr = 308
