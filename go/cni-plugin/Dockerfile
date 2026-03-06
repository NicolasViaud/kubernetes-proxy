FROM golang:1.23-alpine AS builder
WORKDIR /build

COPY go.mod ./
RUN go mod download

COPY . .
# Plugin binary — installed onto the host node by the DaemonSet.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /plugin ./cmd/plugin
# Installer binary — runs inside the DaemonSet container.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /installer ./cmd/installer

# --- runtime ---
# The installer runs as root (it writes to host node directories).
# Use the base distroless image (not :nonroot).
FROM gcr.io/distroless/static-debian12
COPY --from=builder /plugin    /opt/egress-proxy
COPY --from=builder /installer /installer
ENTRYPOINT ["/installer"]
