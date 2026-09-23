package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/shenghuo2/plana-novelai-proxy/internal/proxy"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		if err := healthcheck(); err != nil {
			os.Exit(1)
		}
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	key := os.Getenv("PROXY_SHARED_KEY")
	if len(key) < 32 {
		logger.Error("PROXY_SHARED_KEY must contain at least 32 characters")
		os.Exit(1)
	}

	maxConcurrent := 8
	if raw := os.Getenv("PROXY_MAX_CONCURRENT"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 1000 {
			logger.Error("PROXY_MAX_CONCURRENT must be between 1 and 1000")
			os.Exit(1)
		}
		maxConcurrent = value
	}

	handler, err := proxy.New(proxy.Config{
		SharedKey:     key,
		MaxConcurrent: maxConcurrent,
	})
	if err != nil {
		logger.Error("invalid proxy configuration", "error", err)
		os.Exit(1)
	}

	addr := os.Getenv("PROXY_LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Image generation can take several minutes; the upstream transport
		// bounds the wait for headers while response streaming stays open.
		WriteTimeout: 0,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown failed", "error", err)
		}
	}()

	logger.Info("proxy listening", "address", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func healthcheck() error {
	addr := os.Getenv("PROXY_LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %d", response.StatusCode)
	}
	return nil
}
