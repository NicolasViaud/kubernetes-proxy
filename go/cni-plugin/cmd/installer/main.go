// Package main is the DaemonSet installer for the egress-proxy CNI plugin.
//
// It runs as the main container in the egress-proxy-cni DaemonSet.  On start
// it:
//  1. Copies the plugin binary to the host node's /opt/cni/bin/.
//  2. Finds the primary CNI conflist in /etc/cni/net.d/ and appends the
//     egress-proxy plugin entry to the plugins array (chained mode).
//
// On SIGTERM (pod eviction / DaemonSet deletion) it:
//  1. Restores the original conflist.
//  2. Removes the plugin binary.
//
// Configuration is via environment variables:
//
//	INTERCEPT_NAMESPACES  comma-separated list of namespaces to intercept (default: app)
//	REDIRECT_PORT         port the sidecar listens on                      (default: 15001)
//	PROXY_UID             UID excluded from iptables redirect               (default: 1337)
//	EXCLUDE_PORTS         comma-separated TCP ports to exclude              (default: 53)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	srcBinary  = "/opt/egress-proxy" // path of the binary inside this image
	hostCNIBin = "/host/opt/cni/bin"
	hostCNIConf = "/host/etc/cni/net.d"
	pluginType = "egress-proxy"
)

func main() {
	namespaces := strings.Split(envOr("INTERCEPT_NAMESPACES", "app"), ",")
	redirectPort := envOr("REDIRECT_PORT", "15001")
	proxyUID := envOr("PROXY_UID", "1337")
	excludePorts := strings.Split(envOr("EXCLUDE_PORTS", "53"), ",")

	// --- Install ---

	destBin := filepath.Join(hostCNIBin, pluginType)
	if err := copyFile(srcBinary, destBin, 0755); err != nil {
		log.Fatalf("install binary: %v", err)
	}
	log.Printf("Installed %s", destBin)

	conflistPath, original, err := patchConflist(namespaces, redirectPort, proxyUID, excludePorts)
	if err != nil {
		log.Fatalf("patch conflist: %v", err)
	}
	log.Printf("Patched %s", conflistPath)

	// --- Wait for termination ---

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig

	// --- Cleanup ---

	log.Printf("Removing CNI plugin...")
	if err := os.WriteFile(conflistPath, original, 0644); err != nil {
		log.Printf("restore conflist: %v", err)
	}
	if err := os.Remove(destBin); err != nil {
		log.Printf("remove binary: %v", err)
	}
	log.Printf("Cleanup complete")
}

// patchConflist finds the alphabetically-first .conflist in hostCNIConf,
// appends our plugin entry, and returns the path + the original bytes so
// the caller can restore on exit.
func patchConflist(namespaces []string, redirectPort, proxyUID string, excludePorts []string) (string, []byte, error) {
	matches, err := filepath.Glob(filepath.Join(hostCNIConf, "*.conflist"))
	if err != nil || len(matches) == 0 {
		return "", nil, fmt.Errorf("no .conflist found in %s", hostCNIConf)
	}
	// Alphabetically first = highest-priority conflist (e.g. 10-kindnet.conflist).
	conflistPath := matches[0]

	original, err := os.ReadFile(conflistPath)
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", conflistPath, err)
	}

	// Unmarshal preserving raw JSON for fields we don't touch.
	var conflist map[string]json.RawMessage
	if err := json.Unmarshal(original, &conflist); err != nil {
		return "", nil, fmt.Errorf("parse conflist: %w", err)
	}

	var plugins []json.RawMessage
	if err := json.Unmarshal(conflist["plugins"], &plugins); err != nil {
		return "", nil, fmt.Errorf("parse plugins: %w", err)
	}

	// Idempotent: skip if already present.
	for _, p := range plugins {
		var m map[string]interface{}
		json.Unmarshal(p, &m) //nolint:errcheck
		if m["type"] == pluginType {
			log.Printf("Plugin already present in conflist — skipping patch")
			return conflistPath, original, nil
		}
	}

	// Trim spaces from list values.
	trimmed := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if s := strings.TrimSpace(ns); s != "" {
			trimmed = append(trimmed, s)
		}
	}
	trimmedPorts := make([]string, 0, len(excludePorts))
	for _, p := range excludePorts {
		if s := strings.TrimSpace(p); s != "" {
			trimmedPorts = append(trimmedPorts, s)
		}
	}

	entry, err := json.Marshal(map[string]interface{}{
		"type":                 pluginType,
		"interceptNamespaces": trimmed,
		"redirectPort":        redirectPort,
		"proxyUID":            proxyUID,
		"excludeOutboundPorts": trimmedPorts,
	})
	if err != nil {
		return "", nil, err
	}

	plugins = append(plugins, entry)

	pluginsRaw, err := json.Marshal(plugins)
	if err != nil {
		return "", nil, err
	}
	conflist["plugins"] = pluginsRaw

	patched, err := json.MarshalIndent(conflist, "", "  ")
	if err != nil {
		return "", nil, err
	}

	if err := os.WriteFile(conflistPath, patched, 0644); err != nil {
		return "", nil, fmt.Errorf("write %s: %w", conflistPath, err)
	}

	return conflistPath, original, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
