package aviary

import (
	"bufio"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/tupini07/aviary/internal/controlplane"
)

func TestShutdownDrainsAcceptedHTTPBeforeClosingStore(t *testing.T) {
	av := newTestAviary(t)
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	av.control = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		if _, err := av.CreateProject(r.Context(), "retained", "Retained"); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(201)
	})
	response := httptest.NewRecorder()
	requestDone := make(chan struct{})
	go func() {
		av.ServeHTTP(response, httptest.NewRequest("POST", "http://localhost/", nil))
		close(requestDone)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	av.BeginShutdown()
	rejected := httptest.NewRecorder()
	av.ServeHTTP(rejected, httptest.NewRequest("GET", "http://localhost/", nil))
	if rejected.Code != 503 {
		t.Fatalf("new request status = %d", rejected.Code)
	}
	stopped := make(chan struct{})
	go func() { av.Shutdown(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("closed stores before accepted request finished")
	default:
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete")
	}
	<-requestDone
	if response.Code != 201 {
		t.Fatalf("accepted request failed: %d", response.Code)
	}
	store, err := controlplane.OpenExisting(filepath.Join(av.cfg.DataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Get(context.Background(), "retained"); err != nil {
		t.Fatal("accepted write was not preserved:", err)
	}
}

func TestShutdownDrainsRunningCronAndRecordsItsOutcome(t *testing.T) {
	av := newTestAviary(t)
	session := loginAs(t, av, "admin@example.test", "password123")
	if _, err := av.CreateProject(context.Background(), "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	job := createCron(t, av, "alpha", session, cronJobRequest{Schedule: "@daily", Path: "held"})
	cage, err := av.getCage("alpha", false)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	cage.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		if _, err := cage.app.DB().NewQuery("CREATE TABLE accepted_cron(value TEXT)").Execute(); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	})
	go av.runCronJob("alpha", job.ID)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cron did not start")
	}
	av.BeginShutdown()
	stopped := make(chan struct{})
	go func() { av.Shutdown(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("closed project while cron was running")
	default:
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("cron shutdown did not complete")
	}
	store, err := controlplane.OpenExisting(filepath.Join(av.cfg.DataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.GetCronJob(context.Background(), "alpha", job.ID)
	if err != nil || saved.LastStatus != 204 || saved.LastRunAt == nil {
		t.Fatalf("cron outcome not preserved: %+v, %v", saved, err)
	}
}

func TestShutdownDisconnectsRealtimeWithoutClosingActiveStores(t *testing.T) {
	av := newTestAviary(t)
	if _, err := av.CreateProject(context.Background(), "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(av)
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	request, err := http.NewRequest("GET", server.URL+"/api/realtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "alpha.localhost"
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal("realtime connect:", err)
		}
		if strings.Contains(line, "PB_CONNECT") {
			break
		}
	}
	av.BeginShutdown()
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal("realtime did not close cleanly:", err)
	}
	av.Shutdown()
}

func TestRequiredExistingStateDoesNotBootstrapMissingStores(t *testing.T) {
	parent := t.TempDir()
	if _, err := New(Config{DataDir: filepath.Join(parent, "missing"), RequireExisting: true}); err == nil {
		t.Fatal("bootstrapped missing state")
	}
	av := newTestAviary(t)
	directory := av.cfg.DataDir
	av.Shutdown()
	managed, err := New(Config{DataDir: directory, RequireExisting: true, DisableCron: true})
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Shutdown()
	if managed.cron != nil {
		t.Fatal("isolated rehearsal enabled scheduling")
	}
}

func TestProjectTerminationRunsHooksAndFlushesLogs(t *testing.T) {
	av := newTestAviary(t)
	if _, err := av.CreateProject(context.Background(), "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	cage, err := av.getCage("alpha", false)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	cage.app.OnTerminate().BindFunc(func(event *core.TerminateEvent) error {
		called = true
		return event.Next()
	})
	cage.app.Logger().Warn("accepted before shutdown")
	av.Shutdown()
	if !called {
		t.Fatal("PocketBase termination hooks were skipped")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(av.cfg.DataDir, "projects/alpha/auxiliary.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM _logs WHERE message='accepted before shutdown'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("queued log not flushed: count=%d err=%v", count, err)
	}
}
