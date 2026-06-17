// Command aviary runs many isolated PocketBase projects inside a single
// process, routed by subdomain. It is the proof-of-concept control plane for
// the "many core.App per instance" multi-tenant model.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tupini07/aviary/internal/aviary"
)

// version is the Aviary build version. It is overridden at build time via
// -ldflags "-X main.version=...", e.g. by GoReleaser during a release.
var version = "(untracked)"

func main() {
	// Subcommand dispatch must run before flag.Parse so `aviary update ...`
	// can have its own flags without colliding with the server flags below.
	if len(os.Args) > 1 && os.Args[1] == "update" {
		os.Exit(runUpdate(os.Args[2:], version, os.Stdout))
	}

	addr := flag.String("addr", envOr("AVIARY_ADDR", "127.0.0.1:8090"), "address for the Aviary front server")
	dataDir := flag.String("data", envOr("AVIARY_DATA", "./data"), "parent dir holding control.db and projects/")
	idleTTL := flag.Duration("idle-ttl", envDuration("AVIARY_IDLE_TTL", 5*time.Minute), "evict a project's app after this much inactivity")
	seed := flag.String("seed", envOr("AVIARY_SEED", ""), "comma-separated project ids to auto-provision on startup (dev convenience)")
	allowPBPassword := flag.Bool("allow-dashboard-password", envBool("AVIARY_PB_PASSWORD_LOGIN", false), "keep PocketBase native superuser password login enabled on projects (default: only Aviary-minted token / SSO)")
	showVersion := flag.Bool("version", false, "print the Aviary version and exit")
	flag.Parse()

	if *showVersion {
		log.Printf("aviary %s", version)
		return
	}

	av, err := aviary.New(aviary.Config{
		DataDir:                *dataDir,
		IdleTTL:                *idleTTL,
		Logger:                 slog.Default(),
		AllowDashboardPassword: *allowPBPassword,
	})
	if err != nil {
		log.Fatalf("aviary: init: %v", err)
	}
	defer av.Shutdown()

	cleanupStaleBinary()

	seedProjects(av, *seed)
	bootstrapSuperuser(av)

	printHeader(os.Stdout, version)

	slog.Info("Aviary up", "version", version, "addr", *addr, "data", *dataDir, "idleTTL", idleTTL.String())
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

// printHeader writes an ASCII banner identifying Aviary and its build version
// to out, giving operators an at-a-glance confirmation of what's running.
func printHeader(out io.Writer, version string) {
	const banner = `
    _          _                  
   / \   __   _(_) __ _ _ __ _   _ 
  / _ \  \ \ / / |/ _' | '__| | | |
 / ___ \  \ V /| | (_| | |  | |_| |
/_/   \_\  \_/ |_|\__,_|_|   \__, |
                             |___/ `
	fmt.Fprintln(out, banner)
	fmt.Fprintf(out, "  multi-tenant PocketBase control plane  •  %s\n\n", displayVersion(version))
}


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

// bootstrapSuperuser creates the control-plane superuser from environment
// variables when AVIARY_SUPERUSER_EMAIL and AVIARY_SUPERUSER_PASSWORD are set
// and no superuser exists yet. This enables fully headless, first-run
// provisioning (e.g. in containers) without using the web setup flow.
func bootstrapSuperuser(av *aviary.Aviary) {
	email := strings.TrimSpace(os.Getenv("AVIARY_SUPERUSER_EMAIL"))
	password := os.Getenv("AVIARY_SUPERUSER_PASSWORD")
	if email == "" || password == "" {
		return
	}
	ctx := context.Background()
	has, err := av.HasSuperuser(ctx)
	if err != nil {
		slog.Warn("could not check for existing superuser", "error", err)
		return
	}
	if has {
		return // never overwrite an existing superuser from env
	}
	if err := av.SetSuperuser(ctx, email, password); err != nil {
		slog.Warn("failed to bootstrap superuser from env", "error", err)
		return
	}
	slog.Info("bootstrapped control-plane superuser from environment", "email", email)
}

// cleanupStaleBinary removes the ".old" backup left by a Windows self-update,
// which cannot delete the still-running executable during the update itself.
// Best-effort and a no-op on other platforms / when no backup exists.
func cleanupStaleBinary() {
	exe, err := resolveExecutable()
	if err != nil {
		return
	}
	_ = os.Remove(exe + ".old")
}

// envOr returns the value of the named environment variable, or def if it is
// unset or empty.
func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// envBool parses the named environment variable as a boolean, falling back to
// def if it is unset or invalid.
func envBool(name string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		slog.Warn("invalid bool in environment variable; using default", "var", name, "value", v, "default", def)
		return def
	}
	return b
}

// envDuration parses the named environment variable as a duration, falling back
// to def if it is unset or invalid.
func envDuration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("invalid duration in environment variable; using default", "var", name, "value", v, "default", def.String())
		return def
	}
	return d
}
