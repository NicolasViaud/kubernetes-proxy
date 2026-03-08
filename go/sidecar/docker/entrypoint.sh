#!/bin/bash
# entrypoint.sh — starts rootless Podman service then the sidecar proxy.
#
# Storage driver and user-namespace settings are baked into the image at build
# time via ~/.config/containers/{storage,containers}.conf (see the vfs/ and
# fuse/ sub-directories). This script only handles runtime-specific setup that
# cannot live in the image (writable directories under the emptyDir volume).
#
# Lifecycle:
#   1. Create XDG_RUNTIME_DIR so Podman can write its socket and state files.
#   2. Launch "podman system service" in the background on podman.sock.
#   3. Wait up to 30 s for the socket to appear.
#   4. exec /sidecar — which starts the Docker API proxy (docker.sock) and the
#      egress TCP proxy, both running until the pod terminates.
set -euo pipefail

PODMAN_SOCK=/run/docker/podman.sock
export XDG_RUNTIME_DIR=/tmp/podman-runtime

mkdir -p "$XDG_RUNTIME_DIR"
chmod 0700 "$XDG_RUNTIME_DIR"

# Podman/netavark probe this path for rootless network namespace state.
mkdir -p "$XDG_RUNTIME_DIR/containers/networks/rootless-netns"

echo "[entrypoint] starting rootless podman service..."
podman system service --time=0 "unix://${PODMAN_SOCK}" &

RETRIES=30
until [ -S "$PODMAN_SOCK" ] || [ "$RETRIES" -le 0 ]; do
    sleep 1
    RETRIES=$(( RETRIES - 1 ))
done

if [ ! -S "$PODMAN_SOCK" ]; then
    echo "[entrypoint] ERROR: podman service did not create socket at $PODMAN_SOCK after 30 s" >&2
    exit 1
fi
echo "[entrypoint] podman service ready at $PODMAN_SOCK"

# /sidecar creates /run/docker/docker.sock (Docker API proxy, 0666) and
# listens on :15001 (egress TCP proxy). It replaces this shell as PID 1.
exec /sidecar
