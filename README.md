# kubernetes-proxy

A Go workspace demonstrating **controlled egress** for application pods in a
shared Kubernetes cluster, using a custom transparent sidecar proxy backed by a
central forwarding proxy — without Envoy, without privileged init containers,
and compatible with any primary CNI.

The sidecar also hosts a **rootless Podman daemon** exposed as a Docker API
socket, giving the app container access to `docker build` / `docker run` without
any elevated privileges (see [Rootless Docker sidecar](#rootless-docker-sidecar)
below).

Two CNI modes are supported to set up the iptables interception:

| Mode | How iptables are set up | Sidecar container name |
|---|---|---|
| **Istio CNI** | Istio CNI DaemonSet, chained after kindnet | must be `istio-proxy` |
| **Custom CNI** | Custom CNI plugin DaemonSet, chained after kindnet | any name |

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
│  │  │  (UID 10000) │                  │  (set by CNI plugin) │   │       │
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
| No privileged init container | CNI plugin sets up iptables at the node level (CNI ADD phase) |
| Works with any primary CNI | Both CNI plugins run in *chained* mode after kindnet/flannel/calico |
| No Envoy / Istio control plane per pod | Istio webhook patched to watch a different label; CNI still triggered |
| Restricted Pod Security Admission | All containers: non-root, no capabilities; app uses `RuntimeDefault` seccomp, sidecar uses `Localhost` (`podman.json`) |
| Loop prevention | Sidecar runs as UID 1337; CNI plugin excludes UID 1337 from OUTPUT redirect |
| Shared cluster isolation | `proxy` and `app` namespaces are independent; proxy is not intercepted |
| Custom CNI: no naming constraint | Custom plugin targets any pod in the namespace, regardless of container name |

---

## Repository layout

```
kubernetes-proxy/
├── go.work                     # Go workspace (links all modules)
├── Makefile                    # All common operations
│
├── go/
│   ├── proxy/                  # Central forwarding proxy
│   │   ├── go.mod
│   │   ├── main.go
│   │   └── Dockerfile
│   │
│   ├── sidecar/                # Transparent egress sidecar + Docker API proxy
│   │   ├── go.mod
│   │   ├── main.go             # Listener + CONNECT tunnel + goroutine wiring
│   │   ├── dockerproxy.go      # Docker API proxy (rewrites NetworkMode bridge→host)
│   │   ├── origdst_linux.go    # SO_ORIGINAL_DST (Linux only)
│   │   ├── origdst_other.go    # Stub for non-Linux dev machines
│   │   └── docker/
│   │       ├── entrypoint.sh   # Shared: starts Podman then execs /sidecar
│   │       ├── vfs/            # Variant: restricted PSA (no special devices)
│   │       │   ├── Dockerfile
│   │       │   ├── storage.conf    # driver = vfs + ignore_chown_errors
│   │       │   └── containers.conf # userns = host
│   │       └── fuse/           # Variant: baseline PSA (/dev/fuse required)
│   │           ├── Dockerfile
│   │           ├── storage.conf    # driver = overlay + fuse-overlayfs
│   │           └── containers.conf # userns = host
│   │
│   ├── app/                    # Test application (periodic HTTP client)
│   │   ├── go.mod
│   │   ├── main.go
│   │   └── Dockerfile
│   │
│   └── cni-plugin/             # Custom CNI plugin (alternative to Istio CNI)
│       ├── go.mod
│       ├── Dockerfile
│       ├── cmd/plugin/         # CNI plugin binary (iptables setup)
│       └── cmd/installer/      # DaemonSet installer (patches kindnet conflist)
│
└── k8s/
    ├── kind-config.yaml        # kind cluster (1 control-plane + 2 workers)
    ├── namespaces.yaml         # proxy + app namespaces with PSA labels
    ├── proxy/
    │   ├── deployment.yaml     # 2-replica proxy Deployment
    │   └── service.yaml        # ClusterIP Service on :8080
    ├── app/
    │   └── deployment.yaml     # app + sidecar containers, restricted PSA
    ├── istio/
    │   ├── values-cni.yaml     # Helm values for istio/cni
    │   ├── values-istiod.yaml  # Helm values for istio/istiod
    │   └── patch-webhook.yaml  # Decouple CNI from Envoy injection
    └── cni-plugin/
        └── daemonset.yaml      # Custom CNI plugin DaemonSet
```

---

## Prerequisites

| Tool | Minimum version | Required for |
|---|---|---|
| Go | 1.23 | building images |
| Docker (or Podman) | any recent | building images |
| kind | 0.24 | cluster |
| kubectl | 1.28 | all |
| helm | 3.14 | Istio CNI mode only |

---

## Makefile reference

### Full setup

| Target | Description |
|---|---|
| `make all` | Alias for `all-istio-cni` |
| `make all-istio-cni` | Full setup: cluster → Istio → images → deploy |
| `make all-custom-cni` | Full setup: cluster → images → custom CNI → deploy |

### Cluster

| Target | Description |
|---|---|
| `make cluster-create` | Create the kind cluster |
| `make cluster-delete` | Delete the kind cluster |

### Istio (CNI mode only)

| Target | Description |
|---|---|
| `make helm-add-istio` | Add the Istio Helm repository |
| `make istio-install` | Install Istio base + istiod + CNI, then patch the webhook |

### Images

| Target | Description |
|---|---|
| `make build` | Build app images with vfs sidecar (restricted PSA) |
| `make build-fuse` | Build app images with fuse/overlay sidecar (baseline PSA) |
| `make load` | Load app images into the kind cluster |
| `make push` | `build` + `load` (vfs) |
| `make push-fuse` | `build-fuse` + `load` |
| `make build-cni` | Build the custom CNI plugin image |
| `make load-cni` | Load the CNI plugin image into the kind cluster |
| `make push-cni` | `build-cni` + `load-cni` |
| `make seccomp` | Deploy `k8s/seccomp/podman.json` to all kind nodes via `docker cp` |

### Custom CNI plugin

| Target | Description |
|---|---|
| `make cni-install` | Build, load, and deploy the custom CNI DaemonSet |
| `make cni-uninstall` | Remove the custom CNI DaemonSet (restores original conflist) |

### Kubernetes resources

| Target | Description |
|---|---|
| `make deploy` | Apply all manifests and wait for rollout |
| `make restart` | `kubectl rollout restart` both deployments (no rebuild) |
| `make redeploy` | Alias for `redeploy-istio-cni-vfs` |
| `make redeploy-istio-cni-vfs` | `push` + `restart` |
| `make redeploy-istio-cni-fuse` | `push-fuse` + `restart` |
| `make redeploy-custom-cni-vfs` | `push` + `push-cni` + `restart` |
| `make redeploy-custom-cni-fuse` | `push-fuse` + `push-cni` + `restart` |
| `make undeploy` | Alias for `undeploy-istio-cni` |
| `make undeploy-istio-cni` | Delete app/proxy manifests, keep cluster and Istio |
| `make undeploy-custom-cni` | Uninstall CNI plugin, then delete app/proxy manifests |

### Observability

| Target | Description |
|---|---|
| `make logs-proxy` | Follow proxy container logs |
| `make logs-sidecar` | Follow sidecar container logs (`istio-proxy` container) |
| `make logs-app` | Follow app container logs |
| `make test` | Exec into the app pod and fetch a URL through the proxy |

### Utility

| Target | Description |
|---|---|
| `make clean` | Delete the kind cluster entirely |

---

## Quick start

### Istio CNI mode

Requires helm. The sidecar container **must** be named `istio-proxy`.

```bash
# One-shot: cluster + Istio + images + deploy
make all-istio-cni

# Or step by step:
make cluster-create
make helm-add-istio
make istio-install        # base, istiod, cni, webhook patch
make seccomp              # deploy Localhost seccomp profile to all nodes
make push                 # build + load app images
make deploy
```

### Custom CNI mode

No Istio or helm required. No container naming constraints.

```bash
# One-shot: cluster + images + custom CNI + deploy
make all-custom-cni

# Or step by step:
make cluster-create
make seccomp              # deploy Localhost seccomp profile to all nodes
make push                 # build + load app images
make cni-install          # build, load, and deploy the CNI DaemonSet
make deploy
```

---

## Observe

```bash
# sidecar: intercepted connections with original destination
make logs-sidecar

# proxy: forwarded requests with method, host, status
make logs-proxy

# app: outgoing requests and responses
make logs-app
```

---

## Iterate on code

```bash
# After editing Go source (Istio CNI mode):
make redeploy-istio-cni    # rebuild app images, reload, restart pods

# After editing Go source (custom CNI mode — also rebuilds the CNI plugin):
make redeploy-custom-cni
```

---

## How iptables interception works

Both modes inject the same iptables rules into the pod's network namespace
during the CNI ADD phase (chained after kindnet). The rules redirect all
outbound TCP traffic from non-exempt UIDs to port 15001 where the sidecar
listens.

```
# Outbound — all TCP from UID != 1337 is redirected to port 15001.
iptables -t nat -N ISTIO_OUTPUT
iptables -t nat -A OUTPUT -p tcp -j ISTIO_OUTPUT
iptables -t nat -A ISTIO_OUTPUT -m owner --uid-owner 1337 -j RETURN   # sidecar exempt
iptables -t nat -A ISTIO_OUTPUT -d 127.0.0.0/8 -j RETURN              # loopback exempt
iptables -t nat -A ISTIO_OUTPUT -j REDIRECT --to-ports 15001
```

**Istio CNI** uses `istio-iptables` (part of the Istio distribution) and
triggers on namespaces labelled `istio-injection: enabled`. The Istio webhook
is patched so that Envoy sidecar injection never fires — only the CNI-based
iptables setup does.

**Custom CNI** uses a lightweight Go plugin that applies the same iptables
rules. The installer DaemonSet patches the kindnet conflist to chain the plugin
and restores the original conflist on graceful shutdown.

The sidecar (UID 1337) reads the original destination with
`getsockopt(SO_ORIGINAL_DST)` and opens an HTTP CONNECT tunnel to
`proxy-service.proxy.svc.cluster.local:8080`.

---

## Rootless Docker sidecar

The sidecar container runs a **rootless Podman daemon** and exposes it as a
Docker-compatible Unix socket at `/run/docker/docker.sock` (shared with the app
container via an `emptyDir` volume). The app container can use the standard
`docker` CLI against that socket without any daemon knowledge.

```
app container           sidecar container
(UID 10000)             (UID 1337)

docker CLI ──────────── docker.sock (0666) ──► Docker API proxy
                                                      │
                                               podman.sock (0600)
                                                      │
                                              podman system service
                                             (rootless, user namespace)
```

### Why Podman and not rootless dockerd?

Rootless Docker uses **rootlesskit** which requires `clone()` flag combinations
blocked by the `RuntimeDefault` seccomp profile. Podman writes its uid_map from
*inside* the new user namespace (no external SUID helper), making it compatible
with `restricted` PSA + a `Localhost` seccomp profile.

### Seccomp: why `make seccomp` is required

The `restricted` PSA requires a seccomp profile but `RuntimeDefault` blocks
`clone(CLONE_NEWUSER)` — the syscall Podman needs on startup. The solution is a
`Localhost` profile (`podman.json`), which the `restricted` policy allows.

The profile lives at `k8s/seccomp/podman.json` in the repository. It uses
`SCMP_ACT_ERRNO` as the default action (deny all) and explicitly allows the
standard runtime syscall set plus four extras Podman needs:

| Syscall | Reason |
|---|---|
| `unshare` | Creates Podman's user namespace (`CLONE_NEWUSER`) |
| `clone3` | Modern alternative to `clone`; used by newer Podman/runc versions |
| `mount` | Sets up the container rootfs inside the user namespace |
| `pivot_root` | Switches the container to its own rootfs |

Everything else that `RuntimeDefault` blocks (`kexec_load`, `init_module`,
`bpf`, `ptrace`, `reboot`, …) remains blocked.

#### kind (dev cluster)

`make seccomp` copies `k8s/seccomp/podman.json` to each kind node via
`docker cp` (local Docker, no Kubernetes node access required):

```bash
make seccomp
```

> **Note:** kind node filesystems are ephemeral. Re-run `make seccomp` after
> every cluster restart.

#### Production

Real cluster nodes are VMs — `docker cp` does not apply. Instead, store the
profile in a `ConfigMap` and deploy a privileged `DaemonSet` whose
`initContainer` copies it to the host filesystem via a `hostPath` volume:

```yaml
initContainers:
- name: install-seccomp
  image: busybox
  command: ["sh", "-c", "cp /profile/podman.json /host-seccomp/podman.json"]
  volumeMounts:
  - { name: profile,     mountPath: /profile }
  - { name: seccomp-dir, mountPath: /host-seccomp }
volumes:
- name: profile
  configMap:
    name: podman-seccomp
- name: seccomp-dir
  hostPath:
    path: /var/lib/kubelet/seccomp
    type: DirectoryOrCreate
```

Creating this `DaemonSet` requires cluster-admin (to use `hostPath`), but no
SSH or direct node access.

### Sidecar variants

Two storage driver variants are provided under `go/sidecar/docker/`:

| Variant | Storage driver | PSA level | Extra requirement |
|---|---|---|---|
| `vfs` (default) | Full directory copy per layer | `restricted` | none |
| `fuse` | `fuse-overlayfs` (copy-on-write) | `baseline` | `/dev/fuse` hostPath volume |

Select the variant at build time:

```bash
make build       # vfs (default, restricted PSA)
make build-fuse  # fuse/overlay (baseline PSA)
```

### Verifying from the app container

```bash
kubectl exec -n app deploy/app -c app -- sh

docker images
docker run --rm alpine echo hello
docker run --rm -it alpine sh
```

### Known limitations

| Limitation | Impact |
|---|---|
| No `/etc/subuid` ranges | Container processes always appear as UID 1337 on the host; no per-container UID isolation |
| `vfs` storage driver | Slower image pulls, higher disk usage, no copy-on-write |
| `netns=host` (via proxy rewrite) | All containers share the pod IP; no isolated container networking |
| `Localhost` seccomp must be pre-deployed | `make seccomp` must be re-run after every cluster restart |

---

## Teardown

```bash
# Remove app/proxy manifests only (keep cluster):
make undeploy-istio-cni    # Istio CNI mode
make undeploy-custom-cni   # custom CNI mode (also uninstalls the CNI plugin)

# Delete the kind cluster entirely:
make clean
```
