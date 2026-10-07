package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/tupini07/aviary/internal/aviary"
)

var buildRevision = "development"

func serveAviary(ctx context.Context, listener net.Listener, av *aviary.Aviary, cronEnabled bool) error {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_aviary/health" {
			av.ServeHTTP(w, r)
			return
		}
		projects, err := av.Health(r.Context())
		if err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(struct {
			Status      string `json:"status"`
			Revision    string `json:"revision"`
			Version     string `json:"version"`
			Projects    int    `json:"projects"`
			CronEnabled bool   `json:"cronEnabled"`
		}{"ok", buildRevision, version, projects, cronEnabled}); err != nil {
			slog.Warn("health response failed", "error", err)
		}
	})
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		av.BeginShutdown()
		server.SetKeepAlivesEnabled(false)
		drain, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := server.Shutdown(drain)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("HTTP drain still running; preserving accepted work")
			err = server.Shutdown(context.Background())
		}
		if err != nil {
			return err
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	finished := make(chan struct{})
	go func() {
		av.Shutdown()
		close(finished)
	}()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		slog.Warn("background drain still running; preserving accepted work")
		<-finished
	}
	return nil
}
