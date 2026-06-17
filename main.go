// Command aviary runs many isolated PocketBase projects inside a single
// process, routed by subdomain. It is the proof-of-concept control plane for
// the "many core.App per instance" multi-tenant model.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tupini07/aviary/internal/aviary"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "address for the Aviary front server")
	dataDir := flag.String("data", "./data", "parent dir holding control.db and projects/")
	idleTTL := flag.Duration("idle-ttl", 5*time.Minute, "evict a project's app after this much inactivity")
	seed := flag.String("seed", "", "comma-separated project ids to auto-provision on startup (dev convenience)")
	flag.Parse()

	av, err := aviary.New(aviary.Config{
		DataDir: *dataDir,
		IdleTTL: *idleTTL,
		Logger:  slog.Default(),
	})
	if err != nil {
		log.Fatalf("aviary: init: %v", err)
	}
	defer av.Shutdown()

	seedProjects(av, *seed)

	slog.Info("Aviary up", "addr", *addr, "data", *dataDir, "idleTTL", idleTTL.String())
	slog.Info("control plane", "cmd", "curl -s http://"+*addr+"/")

	srv := &http.Server{
		Addr:              *addr,
		Handler:           av,
		ReadHeaderTimeout: 30 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// seedProjects provisions a comma-separated list of project ids if they don't
// already exist. Intended only as a local-development convenience.
func seedProjects(av *aviary.Aviary, seed string) {
	for _, id := range strings.Split(seed, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		_, err := av.CreateProject(context.Background(), id, id)
		switch {
		case err == nil:
			slog.Info("seeded project", "project", id)
		case errors.Is(err, aviary.ErrExists):
			// already provisioned, nothing to do
		default:
			slog.Warn("failed to seed project", "project", id, "error", err)
		}
	}
}
