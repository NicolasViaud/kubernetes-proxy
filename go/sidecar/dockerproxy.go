// dockerproxy.go — transparent Docker API proxy with network-mode rewriting.
//
// The proxy listens on dockerProxySocket and forwards every request verbatim
// to Podman's socket at podmanSocket, with one exception:
//
//   POST /containers/create
//
// In that request the HostConfig.NetworkMode is rewritten from "bridge" or
// "default" (the docker CLI defaults) to "host". The bridge mode requires
// /dev/net/tun via slirp4netns, which is not available in this pod. Host mode
// reuses the pod's existing network namespace — no extra device needed.
//
// A manual HTTP/1.1 request-response loop is used (not httputil.ReverseProxy)
// because the Docker attach endpoint uses a proprietary upgrade protocol
// ("application/vnd.docker.raw-stream") that the standard reverse proxy does
// not recognise. The loop detects 101 Switching Protocols and hands off to a
// raw bidirectional pipe for the rest of the connection lifetime.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
)

const (
	dockerProxySocket = "/run/docker/docker.sock"
	podmanSocket      = "/run/docker/podman.sock"
)

// startDockerProxy creates dockerProxySocket with 0666 permissions so the app
// container (UID 10000) can connect, then proxies all Docker API requests to
// Podman. Runs until the process exits; call it in a goroutine.
func startDockerProxy(log *slog.Logger) {
	ln, err := net.Listen("unix", dockerProxySocket)
	if err != nil {
		log.Error("docker proxy: listen failed", "socket", dockerProxySocket, "err", err)
		os.Exit(1)
	}
	if err := os.Chmod(dockerProxySocket, 0666); err != nil {
		log.Error("docker proxy: chmod failed", "err", err)
	}

	log.Info("docker proxy ready", "socket", dockerProxySocket, "podman", podmanSocket)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Error("docker proxy: accept error", "err", err)
			continue
		}
		go serveDockerConn(conn, log)
	}
}

// serveDockerConn runs a per-connection HTTP/1.1 proxy loop. For each request:
//  1. Read and optionally rewrite the request.
//  2. Forward to Podman.
//  3. Read and forward the response.
//  4. On 101 Switching Protocols (docker attach / exec), switch to raw pipe.
func serveDockerConn(client net.Conn, log *slog.Logger) {
	defer client.Close()

	server, err := net.Dial("unix", podmanSocket)
	if err != nil {
		log.Error("docker proxy: dial podman", "err", err)
		return
	}
	defer server.Close()

	clientBuf := bufio.NewReader(client)
	serverBuf := bufio.NewReader(server)

	for {
		req, err := http.ReadRequest(clientBuf)
		if err != nil {
			return // client closed or sent invalid data
		}

		// Rewrite NetworkMode for container create.
		if req.Method == http.MethodPost &&
			strings.Contains(req.URL.Path, "/containers/create") {
			body, err := io.ReadAll(req.Body)
			req.Body.Close()
			if err == nil {
				body = rewriteContainerNetwork(body, log)
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
			}
		}

		if err := req.Write(server); err != nil {
			return
		}

		resp, err := http.ReadResponse(serverBuf, req)
		if err != nil {
			return
		}

		// Write the full response (headers + body) to the client.
		// For streaming responses this copies until EOF; for 101 the body is
		// empty and Write returns immediately.
		if err := resp.Write(client); err != nil {
			resp.Body.Close()
			return
		}
		resp.Body.Close()

		// Upgrade: docker attach/exec uses 101 Switching Protocols with a
		// proprietary "application/vnd.docker.raw-stream" body. After the 101
		// response both ends expect raw bytes — switch to a bidirectional pipe.
		if resp.StatusCode == http.StatusSwitchingProtocols {
			errc := make(chan error, 2)
			// clientBuf may have bytes buffered after the last HTTP request;
			// drain it first so Podman sees all client-side stream data.
			go func() { _, err := io.Copy(server, clientBuf); errc <- err }()
			// serverBuf may likewise have bytes buffered after the 101 headers.
			go func() { _, err := io.Copy(client, serverBuf); errc <- err }()
			// Wait for BOTH goroutines. If we return after only one (e.g.
			// because the client has no stdin and closes its write half
			// immediately), we would close the Podman connection before it has
			// finished sending stdout — causing the "broken pipe" error.
			<-errc
			<-errc
			return
		}

		if resp.Close {
			return // server requested connection close
		}
	}
}

// rewriteContainerNetwork changes NetworkMode from "bridge"/"default" to "host"
// in a container-create JSON body, and clears EndpointsConfig so Podman does
// not try to attach the container to a named bridge network.
func rewriteContainerNetwork(body []byte, log *slog.Logger) []byte {
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(body, &cfg); err != nil {
		return body
	}

	hcRaw, ok := cfg["HostConfig"]
	if !ok {
		return body
	}
	var hc map[string]json.RawMessage
	if err := json.Unmarshal(hcRaw, &hc); err != nil {
		return body
	}

	var nm string
	if nmRaw, ok := hc["NetworkMode"]; ok {
		json.Unmarshal(nmRaw, &nm) //nolint:errcheck
	}

	if nm == "" || nm == "bridge" || nm == "default" {
		hc["NetworkMode"] = json.RawMessage(`"host"`)
		hcBytes, err := json.Marshal(hc)
		if err != nil {
			return body
		}
		cfg["HostConfig"] = hcBytes
		cfg["NetworkingConfig"] = json.RawMessage(`{"EndpointsConfig":{}}`)
		log.Debug("docker proxy: rewrote NetworkMode", "from", nm, "to", "host")
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		return body
	}
	return out
}
