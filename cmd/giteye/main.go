package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"giteye/internal/config"
	"giteye/internal/review"
)

func main() {
	path := flag.String("config", "config.yaml", "YAML configuration file (optional; environment variables override it)")
	flag.Parse()
	c, err := config.Load(*path)
	if err != nil {
		slog.Error("configuration invalid", "error", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	server := &http.Server{Addr: c.ListenAddr, Handler: review.NewService(c, logger).Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	logger.Info("Giteye listening", "addr", c.ListenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("HTTP server stopped")
		os.Exit(1)
	}
}
