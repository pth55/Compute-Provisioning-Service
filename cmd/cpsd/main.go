package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cps/internal/api"
	"cps/internal/compute"
	"cps/internal/db"
	"cps/internal/service"
)

func main() {
	defaultAddr := os.Getenv("CPS_ADDR")
	if defaultAddr == "" {
		defaultAddr = "127.0.0.1:8080"
	}
	defaultDataDir := os.Getenv("CPS_DATA_DIR")
	if defaultDataDir == "" {
		defaultDataDir = "."
	}
	addr := flag.String("addr", defaultAddr, "HTTP listen address (default is loopback only)")
	dataDir := flag.String("data-dir", defaultDataDir, "directory containing cps.db, images/, and instances/")
	flag.Parse()

	absoluteDataDir, err := filepath.Abs(*dataDir)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "resolve data directory: %v\n", err)
		os.Exit(1)
	}
	imagesDir := filepath.Join(absoluteDataDir, "images")
	instancesDir := filepath.Join(absoluteDataDir, "instances")
	for _, dir := range []string{absoluteDataDir, imagesDir, instancesDir} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "create CPS data directory %s: %v\n", dir, err)
			os.Exit(1)
		}
	}
	logFile, err := os.OpenFile(filepath.Join(absoluteDataDir, "cps.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "open CPS log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()
	logLevel := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPS_LOG_LEVEL"))) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn", "warning":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, logFile), &slog.HandlerOptions{Level: logLevel, AddSource: true}))
	slog.SetDefault(logger)

	store, err := db.NewStore(filepath.Join(absoluteDataDir, "cps.db"))
	if err != nil {
		logger.Error("initialize persistent SQLite inventory", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Error("close SQLite database", "error", err)
		}
	}()
	hypervisor := compute.NewHypervisor(imagesDir, instancesDir, logger)
	svc := service.New(store, hypervisor, absoluteDataDir, logger)

	// Reconcile persisted rows with real QEMU processes before opening the API.
	// QEMU is deliberately not killed when this HTTP process exits or restarts.
	reconcileCtx, cancelReconcile := context.WithCancel(context.Background())
	defer cancelReconcile()
	logger.Info("startup reconciliation started", "database", filepath.Join(absoluteDataDir, "cps.db"), "log_file", filepath.Join(absoluteDataDir, "cps.log"), "images_dir", imagesDir, "instances_dir", instancesDir)
	svc.Reconcile(reconcileCtx)
	svc.StartReconciler(reconcileCtx, 5*time.Second)

	httpServer := &http.Server{Addr: *addr, Handler: api.NewServer(svc, store, logger).Router(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 6 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("bind CPS HTTP listener", "address", *addr, "error", err)
		os.Exit(1)
	}
	logger.Info("CPS control plane is listening", "address", *addr, "node_id", svc.NodeID, "region", svc.Region, "zone", svc.Zone, "database", filepath.Join(absoluteDataDir, "cps.db"))
	logger.Info("API endpoints registered", "endpoints", "GET /healthz, GET /metadata, GET /images, GET /inventory, GET /events, GET /instances, POST /instances, GET /instances/{id}, PUT /instances/{id}, GET /instances/{id}/logs, POST /instances/{id}/actions, DELETE /instances/{id}")
	logger.Warn("CPS has no API authentication yet; keep the listener bound to 127.0.0.1 unless protected by an authenticated reverse proxy")

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	select {
	case <-signalCtx.Done():
		logger.Info("shutdown signal received; API is stopping, QEMU guests will remain running")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped unexpectedly", "error", err)
		}
	}
	cancelReconcile()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful HTTP shutdown failed", "error", err)
		_ = httpServer.Close()
	}
	logger.Info("CPS API stopped; persistent instance inventory retained")
}
