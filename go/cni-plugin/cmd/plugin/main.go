// Package main is the egress-proxy CNI chained plugin.
//
// It is called by the container runtime (containerd) on every ADD/DEL event,
// after the primary CNI (kindnet) has already set up the pod network.
//
// ADD: if the pod's namespace is in the interceptNamespaces list, the plugin
//      enters the pod network namespace and creates iptables rules that redirect
//      all outbound TCP to port 15001 (where the custom sidecar listens).
//      UID 1337 (the sidecar) is excluded from redirection to prevent loops.
//
// DEL: removes the rules when the pod is deleted.
//
// The plugin reads its configuration from the CNI conflist (stdin):
//
//	{
//	  "type":                 "egress-proxy",
//	  "interceptNamespaces": ["app"],
//	  "redirectPort":        "15001",
//	  "proxyUID":            "1337",
//	  "excludeOutboundPorts": ["53"]
//	}
//
// No external dependencies — only the Go standard library.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// NetConf holds the CNI plugin configuration parsed from stdin.
type NetConf struct {
	CNIVersion string `json:"cniVersion"`
	Name       string `json:"name"`
	Type       string `json:"type"`

	// PrevResult is the result from the previous plugin in the chain.
	// We pass it through unchanged so the runtime can use the IP/route info.
	PrevResult map[string]interface{} `json:"prevResult,omitempty"`

	// Custom fields.
	InterceptNamespaces  []string `json:"interceptNamespaces"`
	RedirectPort         string   `json:"redirectPort"`
	ProxyUID             string   `json:"proxyUID"`
	ExcludeOutboundPorts []string `json:"excludeOutboundPorts"`
}

func main() {
	command := os.Getenv("CNI_COMMAND")

	configBytes, err := io.ReadAll(os.Stdin)
	if err != nil {
		cniErrorf(100, "read stdin: %v", err)
	}

	var conf NetConf
	if err := json.Unmarshal(configBytes, &conf); err != nil {
		cniErrorf(100, "parse config: %v", err)
	}

	if conf.RedirectPort == "" {
		conf.RedirectPort = "15001"
	}
	if conf.ProxyUID == "" {
		conf.ProxyUID = "1337"
	}

	switch command {
	case "ADD":
		cmdAdd(conf)
	case "DEL":
		cmdDel(conf)
	case "CHECK":
		passThrough(conf.PrevResult)
	case "VERSION":
		fmt.Print(`{"cniVersion":"1.0.0","supportedVersions":["0.3.0","0.3.1","0.4.0","1.0.0"]}`)
	default:
		cniErrorf(100, "unknown CNI_COMMAND: %q", command)
	}
}

func cmdAdd(conf NetConf) {
	netns := os.Getenv("CNI_NETNS")
	podNamespace := parsePodNamespace(os.Getenv("CNI_ARGS"))

	if netns != "" && shouldIntercept(podNamespace, conf.InterceptNamespaces) {
		if err := setupIPTables(netns, conf.RedirectPort, conf.ProxyUID, conf.ExcludeOutboundPorts); err != nil {
			cniErrorf(100, "setup iptables for namespace %q: %v", podNamespace, err)
		}
		logf("intercepted namespace=%s netns=%s redirect=:%s", podNamespace, netns, conf.RedirectPort)
	}

	passThrough(conf.PrevResult)
}

func cmdDel(conf NetConf) {
	netns := os.Getenv("CNI_NETNS")
	podNamespace := parsePodNamespace(os.Getenv("CNI_ARGS"))

	if netns != "" && shouldIntercept(podNamespace, conf.InterceptNamespaces) {
		teardownIPTables(netns)
	}

	passThrough(conf.PrevResult)
}

// setupIPTables creates a PROXY_OUTPUT chain in the nat table that redirects
// all outbound TCP to redirectPort, except traffic from proxyUID (the sidecar)
// and loopback traffic.
func setupIPTables(netns, redirectPort, proxyUID string, excludePorts []string) error {
	return inNetNS(netns, func() error {
		rules := [][]string{
			// Create a dedicated chain to make cleanup easy.
			{"-t", "nat", "-N", "PROXY_OUTPUT"},
			// Hook into the OUTPUT chain.
			{"-t", "nat", "-A", "OUTPUT", "-p", "tcp", "-j", "PROXY_OUTPUT"},
			// Sidecar traffic (UID 1337) bypasses redirection — loop prevention.
			{"-t", "nat", "-A", "PROXY_OUTPUT", "-m", "owner", "--uid-owner", proxyUID, "-j", "RETURN"},
			// Loopback is never redirected.
			{"-t", "nat", "-A", "PROXY_OUTPUT", "-d", "127.0.0.0/8", "-j", "RETURN"},
		}

		// Per-port exclusions (e.g. DNS/53 which is UDP anyway, but safe to add).
		for _, port := range excludePorts {
			port = strings.TrimSpace(port)
			if port == "" {
				continue
			}
			rules = append(rules, []string{
				"-t", "nat", "-A", "PROXY_OUTPUT",
				"-p", "tcp", "--dport", port, "-j", "RETURN",
			})
		}

		// Redirect everything else.
		rules = append(rules, []string{
			"-t", "nat", "-A", "PROXY_OUTPUT",
			"-p", "tcp", "-j", "REDIRECT", "--to-ports", redirectPort,
		})

		for _, rule := range rules {
			out, err := exec.Command("iptables", rule...).CombinedOutput()
			if err != nil {
				// "Chain already exists" is not fatal — idempotent re-run.
				if strings.Contains(string(out), "Chain already exists") {
					continue
				}
				return fmt.Errorf("iptables %v: %w\n%s", rule, err, out)
			}
		}
		return nil
	})
}

// teardownIPTables removes the rules added by setupIPTables.
// Errors are ignored — the network namespace may already be gone.
func teardownIPTables(netns string) {
	inNetNS(netns, func() error { //nolint:errcheck
		exec.Command("iptables", "-t", "nat", "-D", "OUTPUT", "-p", "tcp", "-j", "PROXY_OUTPUT").Run()
		exec.Command("iptables", "-t", "nat", "-F", "PROXY_OUTPUT").Run()
		exec.Command("iptables", "-t", "nat", "-X", "PROXY_OUTPUT").Run()
		return nil
	})
}

func shouldIntercept(namespace string, namespaces []string) bool {
	for _, ns := range namespaces {
		if strings.TrimSpace(ns) == namespace {
			return true
		}
	}
	return false
}

// parsePodNamespace extracts K8S_POD_NAMESPACE from the CNI_ARGS env var.
// Format: "K8S_POD_NAME=xxx;K8S_POD_NAMESPACE=yyy;K8S_POD_INFRA_CONTAINER_ID=zzz"
func parsePodNamespace(cniArgs string) string {
	for _, kv := range strings.Split(cniArgs, ";") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 && parts[0] == "K8S_POD_NAMESPACE" {
			return parts[1]
		}
	}
	return ""
}

// passThrough writes the previous CNI result to stdout unchanged.
// The runtime uses this result (IPs, routes, etc.) from the primary CNI.
func passThrough(prevResult map[string]interface{}) {
	if prevResult == nil {
		prevResult = map[string]interface{}{"cniVersion": "1.0.0"}
	}
	out, _ := json.Marshal(prevResult)
	fmt.Print(string(out))
}

// cniErrorf writes a CNI-spec error JSON to stderr and exits 1.
func cniErrorf(code int, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	result := map[string]interface{}{
		"cniVersion": "1.0.0",
		"code":       code,
		"msg":        msg,
	}
	out, _ := json.Marshal(result)
	fmt.Fprint(os.Stderr, string(out))
	os.Exit(1)
}

func logf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[egress-proxy-cni] "+format+"\n", args...)
}
