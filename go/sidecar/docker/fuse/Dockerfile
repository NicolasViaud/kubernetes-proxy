# FUSE variant — for use with baseline Pod Security Admission.
#
# Uses fuse-overlayfs as the overlay storage driver. Requires:
#   - /dev/fuse device (hostPath volume in the pod spec)
#   - baseline PSA (hostPath is blocked under restricted)
#   - Localhost seccomp profile that allows mount() and fuse syscalls
#
# Build context: go/sidecar/
#   docker build -f go/sidecar/docker/fuse/Dockerfile -t sidecar:fuse ./go/sidecar
FROM golang:1.23-alpine AS builder
WORKDIR /build

COPY go.mod ./
RUN go mod download

COPY . .
# Must build for Linux — SO_ORIGINAL_DST is a Linux-only syscall.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /sidecar .

# --- runtime ---
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        uidmap \
        slirp4netns \
        podman \
        # fuse-overlayfs: userspace overlay filesystem via /dev/fuse.
        # Provides copy-on-write layers without CAP_SYS_ADMIN.
        fuse-overlayfs \
    && rm -rf /var/lib/apt/lists/*

# Create user 1337 (matches sidecar.istio.io/proxyUID).
# Remove auto-generated subuid/subgid entries so Podman uses a 1:1 UID mapping
# (host 1337 → UID 0 inside user namespace) instead of calling the SUID
# newuidmap helper, which fails because CAP_SETUID is dropped.
RUN useradd -u 1337 -m -s /bin/bash sidecar \
    && sed -i '/^sidecar:/d' /etc/subuid /etc/subgid

# Bake Podman configuration into the image so the entrypoint needs no cat >.
RUN mkdir -p /home/sidecar/.config/containers
COPY docker/fuse/storage.conf    /home/sidecar/.config/containers/storage.conf
COPY docker/fuse/containers.conf /home/sidecar/.config/containers/containers.conf
RUN chown -R 1337:1337 /home/sidecar/.config

COPY --from=builder /sidecar /sidecar
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

USER 1337
EXPOSE 15001
ENTRYPOINT ["/entrypoint.sh"]
