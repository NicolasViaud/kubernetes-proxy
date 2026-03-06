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

## Makefile reference

| Target | Description |
|---|---|
| `make all` | Full setup from scratch: cluster → Istio → images → deploy |
| `make cluster-create` | Create the kind cluster |
| `make helm-add-istio` | Add the Istio Helm repository |
| `make istio-install` | Install Istio base + istiod + CNI, then patch the webhook |
| `make build` | Build all three Docker images |
| `make load` | Load built images into the kind cluster |
| `make push` | `build` + `load` |
| `make deploy` | Apply all manifests and wait for rollout |
| `make restart` | `kubectl rollout restart` both deployments (no rebuild) |
| `make redeploy` | `push` + `restart` — use after changing Go code |
| `make undeploy` | Delete app/proxy manifests, keep cluster and Istio |
| `make logs-proxy` | Follow proxy container logs |
| `make logs-sidecar` | Follow sidecar container logs |
| `make logs-app` | Follow app container logs |
| `make clean` | Delete the kind cluster entirely |

---

## Quick start

```bash
# One-shot: cluster + Istio + images + deploy
make all

# Watch sidecar intercept traffic
make logs-sidecar

# Watch proxy log forwarded requests
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

Istio ships with **four** `MutatingWebhookConfiguration` webhooks that all
inject the privileged `istio-init` container. The patch
(`k8s/istio/patch-webhook.yaml`) re-targets three of them to a new label
`istio-sidecar-injection: enabled` so they never fire in the `app` namespace.
Istio CNI still watches `istio-injection: enabled` independently and sets up
iptables without injecting Envoy.

| Webhook | Default label | After patch |
|---|---|---|
| `namespace.sidecar-injector.istio.io` | `istio-injection: enabled` | `istio-sidecar-injection: enabled` |
| `rev.namespace.sidecar-injector.istio.io` | `istio-injection: enabled` | `istio-sidecar-injection: enabled` |
| `rev.object.sidecar-injector.istio.io` | `istio-injection: enabled` | `istio-sidecar-injection: enabled` |
| `object.sidecar-injector.istio.io` | `istio-injection: DoesNotExist` | unchanged (never matches `app`) |

### 3 — Build and load images

```bash
make push    # docker build + kind load docker-image
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

### 6 — Iterate on code

```bash
# After editing Go source:
make redeploy    # rebuild images, reload into kind, restart pods
```

### 7 — Ad-hoc curl test

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
