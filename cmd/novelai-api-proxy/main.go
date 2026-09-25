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
	"strings"
	"syscall"
	"time"

	"github.com/shenghuo2/novelai-api-proxy/internal/proxy"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		if err := healthcheck(); err != nil {
			os.Exit(1)
		}
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	queueSize := 64
	if raw := os.Getenv("PROXY_QUEUE_SIZE"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 10000 {
			logger.Error("PROXY_QUEUE_SIZE must be between 1 and 10000")
			os.Exit(1)
		}
		queueSize = value
	}
	quotaTTL := 5 * time.Minute
	if raw := os.Getenv("PROXY_QUOTA_TTL"); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value < time.Minute || value > time.Hour {
			logger.Error("PROXY_QUOTA_TTL must be between 1m and 1h")
			os.Exit(1)
		}
		quotaTTL = value
	}
	statePath := os.Getenv("PROXY_STATE_PATH")
	if statePath == "" {
		statePath = "./data/keys.json"
	}
	handler, err := proxy.NewManaged(proxy.ManagedConfig{
		AdminKey: os.Getenv("PROXY_ADMIN_KEY"), NovelAIToken: os.Getenv("PROXY_NAI_TOKEN"),
		AdminOrigin: os.Getenv("PROXY_ADMIN_ORIGIN"), StatePath: statePath,
		QueueSize: queueSize, QuotaTTL: quotaTTL,
		TrustedProxyCIDRs: strings.Split(os.Getenv("PROXY_TRUSTED_PROXY_CIDRS"), ","),
		LoopbackHostOnly:  os.Getenv("PROXY_BIND_ADDR") == "127.0.0.1",
	})
	if err != nil {
		logger.Error("invalid proxy configuration", "error", err)
		os.Exit(1)
	}
	var appHandler http.Handler = handler
	frontendDir := os.Getenv("PROXY_FRONTEND_DIR")
	if frontendDir == "" {
		if _, err := os.Stat("frontend/dist"); err == nil {
			frontendDir = "frontend/dist"
		} else if !errors.Is(err, os.ErrNotExist) {
			logger.Error("cannot inspect frontend build", "error", err)
			os.Exit(1)
		}
	}
	if frontendDir != "" {
		appHandler, err = mountFrontend(appHandler, frontendDir, handler.AdminUIPath)
		if err != nil {
			logger.Error("invalid frontend build", "error", err)
			os.Exit(1)
		}
		logger.Info("serving frontend", "directory", frontendDir, "path", handler.AdminUIPath())
	}

	addr := os.Getenv("PROXY_LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           appHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Image generation can take several minutes; the upstream transport
		// bounds the wait for headers while response streaming stays open.
		WriteTimeout: 0,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		handler.BeginDrain()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown failed", "error", err)
		}
		if err := handler.WaitActive(shutdownCtx); err != nil {
			logger.Warn("active job did not finish before shutdown", "error", err)
		}
	}()

	logger.Info("proxy listening", "address", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
	if ctx.Err() != nil {
		<-shutdownDone
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
