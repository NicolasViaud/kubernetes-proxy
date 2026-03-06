//go:build linux

package main

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// SO_ORIGINAL_DST is the Linux socket option that returns the original
// destination address of a connection that was redirected by iptables REDIRECT.
const soOriginalDst = 80 // defined in <linux/netfilter_ipv4.h>

// getOriginalDst returns the original destination address (IP:port) of a
// connection that was transparently redirected to this listener by iptables.
//
// It uses getsockopt(SO_ORIGINAL_DST) which is only available on Linux and
// only valid when the connection was redirected by the netfilter REDIRECT
// target (i.e. Istio CNI's iptables rules).
func getOriginalDst(conn net.Conn) (string, error) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return "", fmt.Errorf("not a *net.TCPConn")
	}

	rawConn, err := tc.SyscallConn()
	if err != nil {
		return "", fmt.Errorf("SyscallConn: %w", err)
	}

	// sockaddr_in: sa_family(2) + sin_port(2) + sin_addr(4) + pad(8) = 16 bytes
	var addr syscall.RawSockaddrInet4
	addrLen := uint32(syscall.SizeofSockaddrInet4)

	var sysErr error
	ctrlErr := rawConn.Control(func(fd uintptr) {
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			syscall.IPPROTO_IP,
			soOriginalDst,
			uintptr(unsafe.Pointer(&addr)),
			uintptr(unsafe.Pointer(&addrLen)),
			0,
		)
		if errno != 0 {
			sysErr = errno
		}
	})
	if ctrlErr != nil {
		return "", fmt.Errorf("rawConn.Control: %w", ctrlErr)
	}
	if sysErr != nil {
		return "", fmt.Errorf("getsockopt SO_ORIGINAL_DST: %w", sysErr)
	}

	// addr.Port is in network byte order (big-endian).
	// On a little-endian host we must byte-swap it.
	port := int(addr.Port>>8) | int(addr.Port&0xff)<<8
	ip := net.IP(addr.Addr[:])

	return fmt.Sprintf("%s:%d", ip.String(), port), nil
}
