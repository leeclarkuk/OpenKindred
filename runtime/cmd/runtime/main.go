// Command runtime serves one OpenKindred agent from a resolved configuration file.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/leeclarkuk/openkindred/runtime/internal/app"
)

func main() {
	configPath := flag.String("config", "/etc/openkindred/config.json", "path to the resolved configuration")
	addr := flag.String("addr", ":8080", "listen address")
	shutdownDelay := flag.Duration("shutdown-delay", 5*time.Second, "time to report not ready before closing the listener")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a := app.Start(ctx, app.Options{ConfigPath: *configPath, Getenv: os.Getenv, Logger: log})
	srv := &http.Server{Addr: *addr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", *addr)

	select {
	case err := <-errc:
		log.Error("server stopped", "error", err.Error())
		os.Exit(1)
	case <-ctx.Done():
	}
	a.Drain()
	time.Sleep(*shutdownDelay)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("shutdown", "error", err.Error())
	}
	a.Close()
}
