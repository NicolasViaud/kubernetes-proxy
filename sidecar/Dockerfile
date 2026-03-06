FROM golang:1.23-alpine AS builder
WORKDIR /build

COPY go.mod ./
RUN go mod download

COPY . .
# Must build for Linux — SO_ORIGINAL_DST is a Linux-only syscall.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /sidecar .

# --- runtime ---
# nonroot distroless image: the container will run as UID 65532 by default,
# but the Kubernetes manifest overrides it to 1337 so Istio CNI can exclude
# the sidecar's own traffic from the iptables redirect rules.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /sidecar /sidecar
EXPOSE 15001
ENTRYPOINT ["/sidecar"]
