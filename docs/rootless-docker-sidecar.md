# Rootless Docker daemon as a Kubernetes sidecar

## Goal

Give an application container access to a Docker daemon (for building images,
running containers, etc.) without making the pod privileged.  The daemon runs
in the **sidecar** container in rootless mode; the **app** container consumes
it via a shared Unix socket and the standard `docker` CLI — no daemon knowledge
needed.

---

## Architecture

```
┌─────────────────────── Pod (namespace: app) ──────────────────────────┐
│                                                                         │
│  ┌──────────────────────────┐   DOCKER_HOST   ┌──────────────────────┐ │
│  │   app (UID 10000)        │ ──────────────► │  istio-proxy sidecar │ │
│  │   docker CLI installed   │  unix socket    │  (UID 1337)          │ │
│  │   restricted PSA         │                 │  Podman service      │ │
│  └──────────────────────────┘                 │  + Docker API proxy  │ │
│                                               │  + egress proxy      │ │
│                shared emptyDir: /run/docker/  └──────────────────────┘ │
│                  docker.sock  ← Docker API proxy (written by /sidecar) │
│                  podman.sock  ← Podman REST API (internal)             │
└─────────────────────────────────────────────────────────────────────────┘
```

The sidecar container hosts **three processes**:
1. `podman system service` — rootless Podman, Docker-compatible REST API on
   `/run/docker/podman.sock`.
2. `/sidecar` — Go binary that runs two servers:
   - **Docker API proxy** on `/run/docker/docker.sock`: transparently rewrites
     `NetworkMode: bridge` to `NetworkMode: host` (see Networking section) then
     forwards every request/response to Podman's socket.
   - **Egress TCP proxy** on port 15001: intercepts outbound traffic and
     tunnels it through the central proxy (unchanged from the base
     architecture).

---

## Why Podman instead of rootless Docker?

Standard `dockerd` requires root (`CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, …).
Rootless Docker uses **rootlesskit** + Linux user namespaces, but rootlesskit's
`clone()` flag combinations are blocked by the `RuntimeDefault` seccomp profile
required by the `restricted` Pod Security Admission policy.

Podman avoids rootlesskit entirely: it writes `/proc/self/uid_map` from
*inside* the new user namespace (no external `newuidmap` SUID call needed),
making it compatible with `restricted` PSA + `Localhost` seccomp.

| Runtime | Seccomp needed | CAP_SETUID/GID | PSA level |
|---|---|---|---|
| rootless dockerd | Unconfined | Yes (newuidmap) | privileged |
| rootless Podman | Localhost (custom) | No | **restricted** |

---

## Pod Security Admission

The `app` namespace is labelled `restricted`. All PSA requirements are met:

| Requirement | app container | istio-proxy sidecar |
|---|---|---|
| `allowPrivilegeEscalation: false` | ✓ | ✓ |
| `capabilities: drop: [ALL]` | ✓ | ✓ |
| `runAsNonRoot: true` | ✓ (UID 10000) | ✓ (UID 1337) |
| `seccompProfile` set | RuntimeDefault | **Localhost** (`podman.json`) |

### Why a Localhost seccomp profile for the sidecar?

The `RuntimeDefault` seccomp profile (designed for application containers)
blocks `clone(CLONE_NEWUSER)` — the syscall Podman needs to enter its own user
namespace on startup. A `Localhost` profile is allowed by `restricted` PSA and
lets us grant exactly the extra syscalls Podman needs while keeping deny-by-
default for everything else.

The profile lives at `/var/lib/kubelet/seccomp/podman.json` on every kind node.
In this development cluster it is deployed manually; in production use a
`DaemonSet` with an `initContainer` that writes the profile.

---

## How it works without SUID helpers

Ubuntu's `useradd` normally writes entries to `/etc/subuid` / `/etc/subgid`
which cause Podman to call the SUID `newuidmap` binary. Because `capabilities:
drop: ALL` removes `CAP_SETUID` from the bounding set, that call fails.

The Dockerfile removes the auto-generated entries:

```dockerfile
RUN useradd -u 1337 -m -s /bin/bash sidecar \
    && sed -i '/^sidecar:/d' /etc/subuid /etc/subgid
```

Without subuid ranges Podman uses a 1:1 user-namespace mapping:

```
host UID 1337  ←→  UID 0 (root) inside Podman's user namespace
```

This means all container processes appear as UID 1337 on the host — no
per-container UID isolation, but no SUID helper required.

The storage layer is configured to ignore `lchown` errors (`ignore_chown_errors
= "true"`) so that image layers with files owned by UIDs outside the mapping
can still be extracted.

---

## Networking

`/dev/net/tun` is not available in the pod (it would require a `hostPath`
volume, which `restricted` PSA blocks). Without it, `slirp4netns` (the user-
mode network backend) cannot create isolated network namespaces.

The Docker API proxy in the sidecar rewrites every `POST /containers/create`
request: `HostConfig.NetworkMode` is changed from `"bridge"` / `"default"` to
`"host"`, transparently to the `docker` CLI. Containers therefore share the
pod's network namespace (same IP, same ports).

This means:
- Containers can access the internet through the pod's egress proxy.
- Container traffic is intercepted by the egress sidecar, just like app traffic.
- Containers cannot bind ports that are already used by the pod.
- No isolated container networking (each container shares the pod's IP).

---

## Socket sharing

Two sockets live in the `docker-socket` emptyDir volume at `/run/docker/`:

| Socket | Owner | Permissions | Purpose |
|---|---|---|---|
| `podman.sock` | UID 1337 | 0600 | Podman's internal REST API |
| `docker.sock` | UID 1337 | **0666** | Docker API proxy (app connects here) |

The `/sidecar` binary creates `docker.sock` and sets it to `0666` so UID 10000
(app) can connect without group membership.

---

## Files changed

| File | Change |
|---|---|
| `go/sidecar/Dockerfile` | Ubuntu 24.04 base; install `podman`, `uidmap`, `slirp4netns`; create UID 1337; strip `/etc/subuid` entries |
| `go/sidecar/entrypoint.sh` | Start `podman system service` on `podman.sock`; write `storage.conf` (vfs + ignore_chown_errors) and `containers.conf` (userns=host) |
| `go/sidecar/dockerproxy.go` | New: Docker API proxy that rewrites NetworkMode bridge→host |
| `go/sidecar/main.go` | Start Docker API proxy goroutine |
| `go/app/Dockerfile` | Add `docker-cli` Alpine package |
| `k8s/app/deployment.yaml` | Add emptyDir volumes; `DOCKER_HOST` env; Localhost seccomp profile for sidecar |
| `k8s/namespaces.yaml` | `pod-security.kubernetes.io/enforce: restricted` |

---

## Security context comparison

### app container — fully restricted (unchanged)

```yaml
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: 10000
  capabilities:
    drop: ["ALL"]
  seccompProfile:
    type: RuntimeDefault
```

### sidecar container — restricted with Localhost seccomp

```yaml
securityContext:
  allowPrivilegeEscalation: false   # no_new_privs — compatible with CLONE_NEWUSER
  readOnlyRootFilesystem: false     # Podman writes to $HOME and /tmp
  runAsNonRoot: true
  runAsUser: 1337
  runAsGroup: 1337
  capabilities:
    drop: ["ALL"]                   # no capabilities granted
  seccompProfile:
    type: Localhost
    localhostProfile: podman.json   # allows CLONE_NEWUSER
```

Neither container uses `privileged: true`. The pod satisfies `restricted` PSA.

---

## Deploying the seccomp profile

The `podman.json` profile must exist on every node before the pod schedules.

### kind (dev cluster)

```bash
PROFILE='{"defaultAction":"SCMP_ACT_ALLOW"}'
for node in $(kind get nodes --name kubernetes-proxy); do
  docker exec "$node" bash -c \
    "mkdir -p /var/lib/kubelet/seccomp && echo '$PROFILE' > /var/lib/kubelet/seccomp/podman.json"
done
```

### Production

Deploy a privileged `DaemonSet` with an `initContainer` that writes the profile
to `/var/lib/kubelet/seccomp/podman.json` on each node. The profile can be
stored in a `ConfigMap` and mounted into the init container.

---

## Verifying from the app container

```bash
# Open a shell in the app container
kubectl exec -n app deploy/app -c app -- sh

# List images managed by the rootless daemon
docker images

# Run a container
docker run --rm alpine echo hello

# Run interactively
docker run --rm -it alpine sh

# Build an image (assuming a Dockerfile is present)
docker build -t myimage .
```

---

## Known limitations

| Limitation | Impact |
|---|---|
| No `/etc/subuid` ranges | Container processes on the host always appear as UID 1337; no per-container UID isolation |
| `vfs` storage driver | Slower image pulls; higher disk usage; no copy-on-write |
| `userns=host` | Containers run in Podman's user namespace (UID 0 = host 1337), not an isolated user namespace per container |
| `netns=host` (via proxy rewrite) | All containers share the pod IP; no isolated container networking |
| Localhost seccomp profile required | Must be pre-deployed to every node; see "Deploying the seccomp profile" |
| cgroups V1 | Resource limits are not enforced (kind uses cgroups V1); containers survive only while Podman service runs |
