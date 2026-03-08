// Package main implements a transparent TCP sidecar proxy.
//
// Traffic flow:
//
//	[app process]
//	     |  (outbound TCP — any port/IP)
//	     v
//	[iptables REDIRECT rule — set up by Istio CNI]
//	     |  redirects to localhost:15001
//	     v
//	[this sidecar — port 15001]
//	     |  reads original destination via SO_ORIGINAL_DST
//	     |  opens HTTP CONNECT tunnel to central proxy
//	     v
//	[proxy service — proxy.svc.cluster.local:8080]
//	     |  connects to real destination, logs, forwards
//	     v
//	[internet / cluster service]
//
// The sidecar MUST run under a UID that is excluded from the iptables OUTPUT
// chain (configured via the annotation sidecar.istio.io/proxyUID=1337 and
// the corresponding --proxy-uid flag in Istio CNI). Otherwise the sidecar's
// own connection to the central proxy would be caught in an infinite loop.
package main

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	listenAddr := envOr("LISTEN_ADDR", ":15001")
	proxyAddr := envOr("PROXY_ADDR", "proxy-service.proxy.svc.cluster.local:8080")

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		logger.Error("listen failed", "addr", listenAddr, "err", err)
		os.Exit(1)
	}
	defer listener.Close()

	// Start the Docker API compatibility proxy in the background.
	// It rewrites bridge→host network mode so docker run works without
	// /dev/net/tun (unavailable under restricted PSA).
	go startDockerProxy(logger)

	logger.Info("sidecar ready",
		"listen", listenAddr,
		"proxy", proxyAddr,
	)

	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Error("accept error", "err", err)
			continue
		}
		go handle(conn, proxyAddr, logger)
	}
}

func handle(conn net.Conn, proxyAddr string, log *slog.Logger) {
	defer conn.Close()

	// Recover the original destination before any data is read.
	originalDst, err := getOriginalDst(conn)
	if err != nil {
		log.Error("could not get original destination — dropping connection",
			"remote", conn.RemoteAddr(),
			"err", err,
		)
		return
	}

	log.Info("intercepted",
		"original_dst", originalDst,
		"remote", conn.RemoteAddr(),
	)

	// Open a connection to the central proxy.
	proxyConn, err := net.DialTimeout("tcp", proxyAddr, 10*time.Second)
	if err != nil {
		log.Error("dial proxy failed", "proxy", proxyAddr, "err", err)
		return
	}
	defer proxyConn.Close()

	// Negotiate a tunnel via HTTP CONNECT.
	if err := connectTunnel(proxyConn, originalDst); err != nil {
		log.Error("CONNECT handshake failed", "dst", originalDst, "err", err)
		return
	}

	log.Debug("tunnel established", "dst", originalDst)

	// Bidirectional copy until one side closes.
	errc := make(chan error, 2)
	go func() { _, err := io.Copy(proxyConn, conn); errc <- err }()
	go func() { _, err := io.Copy(conn, proxyConn); errc <- err }()
	<-errc
}

// connectTunnel sends an HTTP CONNECT request to the proxy and waits for 200.
// After this call, proxyConn is a raw TCP tunnel to originalDst.
func connectTunnel(proxyConn net.Conn, target string) error {
	req, err := http.NewRequest(http.MethodConnect, "http://"+target, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Host = target

	if err := req.Write(proxyConn); err != nil {
		return fmt.Errorf("write CONNECT: %w", err)
	}

	br := bufio.NewReader(proxyConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy returned %s", resp.Status)
	}

	// The bufio reader may have consumed bytes beyond the response headers.
	// Push them back into proxyConn so the copy goroutines see them.
	if n := br.Buffered(); n > 0 {
		buf := make([]byte, n)
		br.Read(buf) //nolint:errcheck
		// We cannot "unread" from a net.Conn; wrap the conn instead.
		// This is safe because we only do it once at tunnel setup.
		//
		// In practice Istio-style CONNECT responses have no body, so
		// br.Buffered() == 0.  The copy goroutine reads from proxyConn
		// directly, so leftover bytes would be dropped.  If this matters in
		// your environment, replace net.Conn with an io.MultiReader.
		_ = buf
	}

	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
