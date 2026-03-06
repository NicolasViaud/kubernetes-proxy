package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := envOr("LISTEN_ADDR", ":8080")

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	server := &http.Server{
		Addr:         addr,
		Handler:      &proxyHandler{logger: logger},
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // no write timeout: CONNECT tunnels are long-lived
	}

	logger.Info("proxy starting", "addr", addr)
	if err := server.ListenAndServe(); err != nil {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

type proxyHandler struct {
	logger *slog.Logger
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.logger.Info("incoming request",
		"method", r.Method,
		"host", r.Host,
		"uri", r.RequestURI,
		"remote", r.RemoteAddr,
		"user_agent", r.UserAgent(),
	)

	if r.Method == http.MethodConnect {
		h.handleConnect(w, r)
		return
	}
	h.handleHTTP(w, r)
}

// handleConnect handles the HTTP CONNECT method used for tunneling (HTTPS and
// our custom sidecar-to-proxy protocol).
func (h *proxyHandler) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host

	dest, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		h.logger.Error("CONNECT: dial failed", "target", target, "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer dest.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, brw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Signal the client that the tunnel is established.
	fmt.Fprintf(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")

	// If the hijacker already buffered bytes from the client, flush them.
	if brw.Reader.Buffered() > 0 {
		buf := make([]byte, brw.Reader.Buffered())
		brw.Read(buf) //nolint:errcheck
		dest.Write(buf) //nolint:errcheck
	}

	errc := make(chan error, 2)
	go func() { _, err := io.Copy(dest, clientConn); errc <- err }()
	go func() { _, err := io.Copy(clientConn, dest); errc <- err }()
	<-errc

	h.logger.Info("CONNECT: tunnel closed", "target", target)
}

// handleHTTP forwards plain HTTP requests on behalf of the client.
func (h *proxyHandler) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip the RequestURI so http.DefaultTransport builds it from r.URL.
	r.RequestURI = ""

	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		h.logger.Error("HTTP: forward failed", "url", r.URL.String(), "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	h.logger.Info("HTTP: forwarded",
		"url", r.URL.String(),
		"status", resp.StatusCode,
	)

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
