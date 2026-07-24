package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func testCLIContext(t *testing.T, cwd string, env map[string]string) (cliContext, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return cliContext{
		cwd:           cwd,
		userConfigDir: filepath.Join(t.TempDir(), "user-config"),
		getenv: func(name string) string {
			return env[name]
		},
		stdin:  strings.NewReader(""),
		stdout: stdout,
		stderr: stderr,
	}, stdout, stderr
}

func TestProjectInitWritesCommitSafeConfig(t *testing.T) {
	root := t.TempDir()
	ctx, stdout, stderr := testCLIContext(t, root, map[string]string{"AVIARY_KEY": "av_must_not_be_written"})
	code := runProject([]string{"init", "--url", "https://console.example.com/", "--project", "site", "--dir", "build"}, ctx)
	if code != 0 {
		t.Fatalf("runProject init = %d; stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(root, projectConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("av_must_not_be_written")) {
		t.Fatal("project config contains the API key")
	}
	var cfg projectConfig
	if err := decodeStrictJSON(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "https://console.example.com" || cfg.Project != "site" || cfg.Directory != "build" {
		t.Fatalf("config = %+v", cfg)
	}
	if !strings.Contains(stdout.String(), "Initialized") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	stderr.Reset()
	if code := runProject([]string{"init", "--url", cfg.URL, "--project", cfg.Project}, ctx); code == 0 {
		t.Fatal("second init unexpectedly overwrote the project config")
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Fatalf("overwrite error = %q", stderr.String())
	}
}

func TestFindProjectConfigWalksParents(t *testing.T) {
	root := t.TempDir()
	want := projectConfig{URL: "https://console.example.com", Project: "site", Directory: "dist"}
	if err := writeProjectConfig(filepath.Join(root, projectConfigName), want, false); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "src", "components")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, gotRoot, err := findProjectConfig(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || gotRoot != root {
		t.Fatalf("findProjectConfig = %+v, %q; want %+v, %q", got, gotRoot, want, root)
	}
}

func TestProjectCredentialsSetAndRemove(t *testing.T) {
	root := t.TempDir()
	if err := writeProjectConfig(filepath.Join(root, projectConfigName), projectConfig{
		URL: "https://console.example.com", Project: "site", Directory: "dist",
	}, false); err != nil {
		t.Fatal(err)
	}

	ctx, stdout, stderr := testCLIContext(t, root, nil)
	ctx.stdin = strings.NewReader("av_local_secret\n")
	if code := runProject([]string{"credentials", "set"}, ctx); code != 0 {
		t.Fatalf("credentials set = %d; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "av_local_secret") || strings.Contains(stderr.String(), "av_local_secret") {
		t.Fatal("credential command printed the API key")
	}
	key, err := lookupCredential(ctx, "https://console.example.com", "site")
	if err != nil {
		t.Fatal(err)
	}
	if key != "av_local_secret" {
		t.Fatalf("stored key = %q", key)
	}
	path := filepath.Join(ctx.userConfigDir, "aviary", "config.json")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("credential config mode = %o, want 600", info.Mode().Perm())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := runProject([]string{"credentials", "remove"}, ctx); code != 0 {
		t.Fatalf("credentials remove = %d; stderr=%s", code, stderr.String())
	}
	key, err = lookupCredential(ctx, "https://console.example.com", "site")
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		t.Fatalf("key still stored: %q", key)
	}
}

func TestConcurrentCredentialUpdatesPreserveAllEntries(t *testing.T) {
	root := t.TempDir()
	ctx, _, _ := testCLIContext(t, root, nil)
	const count = 16
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := saveCredential(ctx, storedCredential{
				URL:     "https://console.example.com",
				Project: fmt.Sprintf("site-%d", index),
				APIKey:  fmt.Sprintf("av_key_%d", index),
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, _, err := loadUserConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Credentials) != count {
		t.Fatalf("stored %d credentials, want %d", len(cfg.Credentials), count)
	}
}

func TestNormalizeConsoleURL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://console.example.com/", "https://console.example.com", true},
		{"http://localhost:8090", "http://localhost:8090", true},
		{"console.example.com", "", false},
		{"https://console.example.com/prefix", "", false},
		{"https://console.example.com?x=1", "", false},
	} {
		got, err := normalizeConsoleURL(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("normalizeConsoleURL(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}
