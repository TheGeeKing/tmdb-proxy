package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/valkey-io/valkey-go"
)

func main() {
	if err := run(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}
func run() error {
	if err := loadDotEnv(".env"); err != nil {
		return err
	}
	cfg, err := envConfig()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.level}))
	cache, err := valkey.NewClient(cfg.valkey)
	if err != nil {
		return errors.New("cannot connect to Valkey; check VALKEY_URL and server availability")
	}
	defer cache.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := newRelay(ctx, cfg, cache, logger)
	server := &http.Server{Addr: cfg.listen, Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: (time.Duration(cfg.tmdbQueueSize+1) * time.Second / time.Duration(cfg.tmdbRPS)) + cfg.tmdbTimeout + 3*cfg.valkeyTimeout + 10*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	done := make(chan error, 1)
	go func() { logger.Info("tmdb-relay listening", "address", cfg.listen); done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("HTTP server failed to listen")
		}
		return nil
	case <-stop:
		logger.Info("shutting down")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), (time.Duration(cfg.tmdbQueueSize+1)*time.Second/time.Duration(cfg.tmdbRPS))+cfg.tmdbTimeout+3*cfg.valkeyTimeout+5*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			cancel()
			_ = server.Close()
			return errors.New("HTTP shutdown deadline exceeded")
		}
		return nil
	}
}
