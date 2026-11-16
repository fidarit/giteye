package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	if err := run(); err != nil {
		slog.Error("service stopped", "cause", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("config", "config.yaml", "YAML configuration file (optional; environment variables override it)")
	flag.Parse()
	c, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("configuration invalid: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	service, err := review.NewService(c, logger)
	if err != nil {
		return fmt.Errorf("initialize service: %w", err)
	}
	server := &http.Server{
		Addr:              c.ListenAddr,
		Handler:           service.Handler(),
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownErr := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr <- errors.Join(server.Shutdown(shutdown), service.Shutdown(shutdown))
	}()
	logger.Info("Giteye listening", "addr", c.ListenAddr)
	serveErr := server.ListenAndServe()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		stop()
		shutdownErr := <-shutdownErr
		return errors.Join(fmt.Errorf("HTTP server stopped: %w", serveErr), shutdownErr)
	}
	if err := <-shutdownErr; err != nil {
		return fmt.Errorf("shut down HTTP server: %w", err)
	}
	return nil
}
