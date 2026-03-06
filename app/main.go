// Package main is a simple HTTP test client.
//
// It fires periodic GET requests to configurable target URLs so you can
// observe the proxy and sidecar logs in the cluster.
//
// Environment variables:
//
//	TARGETS   comma-separated list of URLs (default: http://httpbin.org/get,https://httpbin.org/get)
//	INTERVAL  time between rounds (default: 10s)
package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	targets := strings.Split(envOr("TARGETS", "http://httpbin.org/get,https://httpbin.org/get"), ",")
	interval := mustParseDuration(envOr("INTERVAL", "10s"))

	client := &http.Client{Timeout: 15 * time.Second}

	logger.Info("app starting",
		"targets", targets,
		"interval", interval,
	)

	for {
		for _, target := range targets {
			target = strings.TrimSpace(target)
			if target == "" {
				continue
			}

			logger.Info("sending request", "url", target)

			resp, err := client.Get(target)
			if err != nil {
				logger.Error("request failed", "url", target, "err", err)
				continue
			}

			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			logger.Info("response received",
				"url", target,
				"status", resp.StatusCode,
				"bytes", len(body),
			)
		}

		time.Sleep(interval)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustParseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic("invalid INTERVAL: " + err.Error())
	}
	return d
}
