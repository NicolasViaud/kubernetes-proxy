FROM golang:1.23-alpine AS builder
WORKDIR /build

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /proxy .

# --- runtime ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /proxy /proxy
EXPOSE 8080
ENTRYPOINT ["/proxy"]
