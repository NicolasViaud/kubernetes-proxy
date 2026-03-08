FROM golang:1.23-alpine AS builder
WORKDIR /build

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /app .

# --- runtime ---
# Use alpine so you can exec a shell and run curl for manual testing.
# Switch to distroless for production: gcr.io/distroless/static-debian12:nonroot
FROM alpine:3.21
# docker-cli provides the `docker` binary so the app can talk to the rootless
# dockerd running in the sidecar container via the shared Unix socket.
RUN apk add --no-cache curl docker-cli
# Run as non-root to satisfy the restricted PSA (matches deployment runAsUser: 10000).
RUN adduser -D -u 10000 app
USER 10000
COPY --from=builder /app /app
ENTRYPOINT ["/app"]
