package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/tupini07/aviary/internal/aviary"
)

func TestManagedHealthAndCancellation(t *testing.T) {
	av, err := aviary.New(aviary.Config{DataDir: t.TempDir(), DisableCron: true})
	if err != nil {
		t.Fatal(err)
	}
	defer av.Shutdown()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveAviary(ctx, listener, av, false) }()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/_aviary/health")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var health struct {
		Status      string `json:"status"`
		Revision    string `json:"revision"`
		Projects    int    `json:"projects"`
		CronEnabled bool   `json:"cronEnabled"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || health.Status != "ok" || health.Revision != buildRevision ||
		health.Projects != 0 || health.CronEnabled || len(response.Cookies()) != 0 {
		t.Fatalf("unexpected health: %s", body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server cancellation did not drain")
	}
}

func TestRuntimeSignalHelper(t *testing.T) {
	if os.Getenv("AVIARY_TEST_HELPER") != "1" {
		return
	}
	os.Args = []string{"aviary", "--addr", os.Getenv("AVIARY_TEST_ADDR"),
		"--data", os.Getenv("AVIARY_TEST_DATA"), "--require-existing", "--disable-cron"}
	main()
}

func TestActualSIGTERMAndOccupiedPort(t *testing.T) {
	directory := t.TempDir()
	av, err := aviary.New(aviary.Config{DataDir: directory, DisableCron: true})
	if err != nil {
		t.Fatal(err)
	}
	av.Shutdown()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	missing := filepath.Join(t.TempDir(), "not-created")
	command := func(data string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeSignalHelper$")
		cmd.Env = append(os.Environ(), "AVIARY_TEST_HELPER=1", "AVIARY_TEST_ADDR="+address,
			"AVIARY_TEST_DATA="+data)
		return cmd
	}
	if output, err := command(missing).CombinedOutput(); err == nil {
		t.Fatalf("occupied port succeeded: %s", output)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("port failure bootstrapped state")
	}
	listener.Close()
	cmd := command(directory)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		waited <- cmd.Wait()
		close(finished)
	}()
	defer func() {
		select {
		case <-finished:
		default:
			_ = cmd.Process.Kill()
			<-finished
		}
	}()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/_aviary/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("helper not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal("SIGTERM did not exit cleanly:", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not finish")
	}
}
