# kubernetes-proxy

A Go workspace demonstrating **controlled egress** for application pods in a
shared Kubernetes cluster, using a custom transparent sidecar proxy backed by a
central forwarding proxy — without Envoy, without privileged init containers,
and compatible with any primary CNI.

---

## Architecture

```
┌─────────────────────────── namespace: app ──────────────────────────────┐
│                                                                          │
│  ┌──────────────────────────────────────────────────────────────┐       │
│  │  Pod (restricted PSA — no privileges, UID 10000 / 1337)      │       │
│  │                                                               │       │
│  │  ┌──────────────┐   outbound TCP   ┌─────────────────────┐   │       │
│  │  │   app        │ ──────────────→  │  iptables REDIRECT  │   │       │
│  │  │  (UID 10000) │                  │  (set by Istio CNI) │   │       │
│  │  └──────────────┘                  └──────────┬──────────┘   │       │
│  │                                               │               │       │
│  │                                     redirect to :15001        │       │
│  │                                               ▼               │       │
│  │                                   ┌───────────────────────┐   │       │
│  │                                   │  sidecar (UID 1337)   │   │       │
│  │                                   │  port 15001           │   │       │
│  │                                   │  SO_ORIGINAL_DST      │   │       │
│  │                                   │  HTTP CONNECT tunnel  │   │       │
│  │                                   └──────────┬────────────┘   │       │
│  └─────────────────────────────────────────────┼────────────────┘       │
│                                                 │ UID 1337 → NOT         │
│                                                 │ redirected by iptables │
└─────────────────────────────────────────────────┼────────────────────────┘
                                                  │
                           ClusterIP: proxy-service.proxy.svc:8080
                                                  │
┌─────────────────────────── namespace: proxy ────┼────────────────────────┐
│                                                  ▼                        │
│              ┌───────────────────────────────────────────┐               │
│              │  proxy Deployment  (2 replicas)            │               │
│              │  • logs method / host / remote             │               │
│              │  • HTTP CONNECT → dials real destination   │               │
│              │  • forwards plain HTTP requests            │               │
│              └───────────────────────────────────────────┘               │
└──────────────────────────────────────────────────────────────────────────┘
                                     │
                              internet / cluster
```

### Key design decisions

| Concern | Solution |
|---|---|
| No privileged init container | Istio CNI sets up iptables at the node level (CNI ADD phase) |
| Works with any primary CNI | Istio CNI runs in *chained* mode after kindnet/flannel/calico |
| No Envoy / Istio control plane per pod | Webhook patched to watch a different label; CNI still triggered |
| Restricted Pod Security Admission | All containers: non-root, no capabilities, seccomp RuntimeDefault |
| Loop prevention | Sidecar runs as UID 1337; Istio CNI excludes UID 1337 from OUTPUT redirect |
| Shared cluster isolation | `proxy` and `app` namespaces are independent; proxy is not intercepted |

---

## Repository layout

```
kubernetes-proxy/
├── go.work                     # Go workspace (links all three modules)
├── Makefile                    # All common operations
│
├── proxy/                      # Central forwarding proxy
│   ├── go.mod
│   ├── main.go
│   └── Dockerfile
│
├── sidecar/                    # Transparent egress sidecar
│   ├── go.mod
│   ├── main.go                 # Listener + CONNECT tunnel logic
│   ├── origdst_linux.go        # SO_ORIGINAL_DST (Linux only)
│   ├── origdst_other.go        # Stub for non-Linux dev machines
│   └── Dockerfile
│
├── app/                        # Test application (periodic HTTP client)
│   ├── go.mod
│   ├── main.go
│   └── Dockerfile
│
└── k8s/
    ├── kind-config.yaml        # kind cluster (1 control-plane + 2 workers)
    ├── namespaces.yaml         # proxy + app namespaces with PSA labels
    ├── proxy/
    │   ├── deployment.yaml     # 2-replica proxy Deployment
    │   └── service.yaml        # ClusterIP Service on :8080
    ├── app/
    │   └── deployment.yaml     # app + sidecar containers, restricted PSA
    └── istio/
        ├── values-cni.yaml     # Helm values for istio/cni
        ├── values-istiod.yaml  # Helm values for istio/istiod
        └── patch-webhook.yaml  # Decouple CNI from Envoy injection
```

---

## Prerequisites

| Tool | Minimum version |
|---|---|
| Go | 1.23 |
| Docker (or Podman) | any recent |
| kind | 0.24 |
| kubectl | 1.28 |
| helm | 3.14 |

---

## Quick start

```bash
# 1. One-shot: cluster + Istio + images + deploy
make all

# 2. Watch sidecar intercept traffic
make logs-sidecar

# 3. Watch proxy log forwarded requests
make logs-proxy
```

Or step by step:

### 1 — Create the kind cluster

```bash
make cluster-create
```

### 2 — Install Istio (CNI + istiod) and patch the webhook

```bash
make helm-add-istio
make istio-install        # installs base, istiod, cni, patches webhook
```

The patch changes the `MutatingWebhookConfiguration` so the sidecar-injector
webhook fires only on namespaces labelled `istio-sidecar-injection: enabled`,
while Istio CNI continues to watch `istio-injection: enabled`. The `app`
namespace has only the CNI label, so iptables is set up but Envoy is never
injected.

### 3 — Build and load images

```bash
make push    # build + kind load docker-image
```

### 4 — Deploy

```bash
make deploy
```

### 5 — Observe

```bash
# sidecar: intercepted connections with original destination
make logs-sidecar

# proxy: forwarded requests with method, host, status
make logs-proxy

# app: outgoing requests and responses
make logs-app
```

### 6 — Ad-hoc curl test

```bash
make test
```

This spawns a temporary `curl` pod in the `app` namespace.  The pod's traffic
is redirected by Istio CNI to port 15001 — but wait, `kubectl run` does not
go through CNI at annotation-apply time in a running cluster; use the full
app Deployment for the live traffic path.  For a quick smoke-test without
CNI interception:

```bash
kubectl exec -n app deploy/app -c app -- wget -qO- http://httpbin.org/get
```

---

## How iptables interception works

Istio CNI runs as a DaemonSet on every node.  When a pod is admitted to a
namespace labelled `istio-injection: enabled`, the CNI plugin (chained after
kindnet) enters the pod's network namespace and runs `istio-iptables` which
creates rules equivalent to:

```
# Outbound — all TCP from UID != 1337 is redirected to port 15001.
iptables -t nat -N ISTIO_OUTPUT
iptables -t nat -A OUTPUT -p tcp -j ISTIO_OUTPUT
iptables -t nat -A ISTIO_OUTPUT -m owner --uid-owner 1337 -j RETURN   # sidecar exempt
iptables -t nat -A ISTIO_OUTPUT -d 127.0.0.0/8 -j RETURN              # loopback exempt
iptables -t nat -A ISTIO_OUTPUT -j REDIRECT --to-ports 15001
```

The sidecar (UID 1337) reads the original destination with
`getsockopt(SO_ORIGINAL_DST)` and opens an HTTP CONNECT tunnel to
`proxy-service.proxy.svc.cluster.local:8080`.

---

## Extending the proxy

The proxy is intentionally minimal — it logs and forwards.  Natural next steps:

- **Allow-list / deny-list**: reject CONNECT requests to unauthorized hosts.
- **mTLS between sidecar and proxy**: use `crypto/tls` with cluster-local certs.
- **Metrics**: expose a Prometheus `/metrics` endpoint with request counts.
- **SOCKS5**: replace HTTP CONNECT with SOCKS5 in the sidecar for non-HTTP traffic.

---

## Teardown

```bash
make clean    # deletes the kind cluster
```
