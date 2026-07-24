package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	aviaryserver "github.com/tupini07/aviary/internal/aviary"
)

func TestWriteDeployArchiveUsesDirectoryContentsAsRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("js"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, total, err := buildDeployManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if total != 6 {
		t.Fatalf("total = %d, want 6", total)
	}
	var archive bytes.Buffer
	if err := writeDeployArchive(&archive, manifest); err != nil {
		t.Fatal(err)
	}
	files := readTGZ(t, archive.Bytes())
	if string(files["index.html"]) != "home" || string(files["assets/app.js"]) != "js" {
		t.Fatalf("archive files = %#v", files)
	}
	if _, ok := files[filepath.Base(root)+"/index.html"]; ok {
		t.Fatal("archive incorrectly included the build directory itself")
	}
}

func TestBuildDeployManifestRejectsEmptyDirectory(t *testing.T) {
	if _, _, err := buildDeployManifest(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no regular files") {
		t.Fatalf("empty manifest error = %v", err)
	}
}

func TestWriteDeployArchiveRejectsChangedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "index.html")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := buildDeployManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	err = writeDeployArchive(&archive, manifest)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed-file error = %v", err)
	}
}

func TestRunDeployUsesConfigAndEnvironmentKey(t *testing.T) {
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(filepath.Join(dist, "css"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "css", "app.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	var requestErr string
	var requestMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		defer requestMu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects/site/deploy" {
			requestErr = r.Method + " " + r.URL.Path
		}
		if r.Header.Get("Authorization") != "Bearer av_ci_secret" {
			requestErr = "bad authorization"
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			requestErr = err.Error()
		} else {
			files, err := parseTGZ(data)
			if err != nil {
				requestErr = err.Error()
			} else if string(files["index.html"]) != "hello" || string(files["css/app.css"]) != "body{}" {
				requestErr = "bad archive contents"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"mode":"replace","files":2,"bytes":11}`)
	}))
	defer server.Close()

	if err := writeProjectConfig(filepath.Join(root, projectConfigName), projectConfig{
		URL: server.URL, Project: "site", Directory: "dist",
	}, false); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, stdout, stderr := testCLIContext(t, nested, map[string]string{"AVIARY_KEY": "av_ci_secret"})
	ctx.httpClient = server.Client()
	if code := runDeploy(nil, ctx); code != 0 {
		t.Fatalf("runDeploy = %d; stderr=%s", code, stderr.String())
	}
	requestMu.Lock()
	defer requestMu.Unlock()
	if requestErr != "" {
		t.Fatal(requestErr)
	}
	if !strings.Contains(stdout.String(), "Deployed 2 files") || strings.Contains(stdout.String(), "av_ci_secret") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunDeployReportsServerErrorWithoutSecret(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInsufficientStorage)
		_, _ = io.WriteString(w, `{"error":"storage quota exceeded","code":507}`)
	}))
	defer server.Close()
	ctx, _, stderr := testCLIContext(t, root, map[string]string{"AVIARY_KEY": "av_do_not_print"})
	ctx.httpClient = server.Client()
	code := runDeploy([]string{"--url", server.URL, "--project", "site", "."}, ctx)
	if code != 1 || !strings.Contains(stderr.String(), "storage quota exceeded") {
		t.Fatalf("runDeploy = %d; stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "av_do_not_print") {
		t.Fatal("error output contains the API key")
	}
}

func TestResolveDeploySettingsPrecedenceAndStoredKey(t *testing.T) {
	root := t.TempDir()
	if err := writeProjectConfig(filepath.Join(root, projectConfigName), projectConfig{
		URL: "https://config.example.com", Project: "config-project", Directory: "config-dist",
	}, false); err != nil {
		t.Fatal(err)
	}
	ctx, _, _ := testCLIContext(t, root, map[string]string{
		"AVIARY_URL":     "https://env.example.com",
		"AVIARY_PROJECT": "env-project",
		"AVIARY_DIR":     "env-dist",
		"AVIARY_KEY":     "av_env",
	})
	settings, err := resolveDeploySettings(ctx, "https://flag.example.com", "flag-project", "flag-dist", "positional-dist", "av_flag", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if settings.URL != "https://flag.example.com" || settings.Project != "flag-project" ||
		settings.APIKey != "av_flag" || settings.Directory != filepath.Join(root, "positional-dist") {
		t.Fatalf("settings = %+v", settings)
	}

	ctx.getenv = func(string) string { return "" }
	if _, err := saveCredential(ctx, storedCredential{
		URL: "https://config.example.com", Project: "config-project", APIKey: "av_stored",
	}); err != nil {
		t.Fatal(err)
	}
	settings, err = resolveDeploySettings(ctx, "", "", "", "", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if settings.APIKey != "av_stored" || settings.Directory != filepath.Join(root, "config-dist") {
		t.Fatalf("stored settings = %+v", settings)
	}
}

func TestRunDeployHonorsTimeout(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, `{"mode":"replace","files":1,"bytes":1}`)
	}))
	defer server.Close()
	ctx, _, stderr := testCLIContext(t, root, map[string]string{"AVIARY_KEY": "av_timeout"})
	ctx.httpClient = server.Client()
	code := runDeploy([]string{"--url", server.URL, "--project", "site", "--timeout", "10ms", "."}, ctx)
	if code != 1 || !strings.Contains(stderr.String(), "deadline exceeded") {
		t.Fatalf("runDeploy = %d; stderr=%q", code, stderr.String())
	}
}

func TestRunDeployReplacesLiveArtifactsEndToEnd(t *testing.T) {
	av, err := aviaryserver.New(aviaryserver.Config{
		DataDir: t.TempDir(),
		IdleTTL: time.Minute,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer av.Shutdown()
	if err := av.SetSuperuser(t.Context(), "admin@example.com", "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := av.CreateProject(t.Context(), "site", "Site"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(av)
	defer server.Close()
	key := mintProjectKey(t, server)

	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeProjectConfig(filepath.Join(root, projectConfigName), projectConfig{
		URL: server.URL, Project: "site", Directory: "dist",
	}, false); err != nil {
		t.Fatal(err)
	}
	ctx, _, stderr := testCLIContext(t, root, map[string]string{"AVIARY_KEY": key})
	ctx.httpClient = server.Client()

	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runDeploy(nil, ctx); code != 0 {
		t.Fatalf("first deploy = %d; stderr=%s", code, stderr.String())
	}
	if err := os.Remove(filepath.Join(dist, "stale.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := runDeploy(nil, ctx); code != 0 {
		t.Fatalf("second deploy = %d; stderr=%s", code, stderr.String())
	}

	if status, body := getProjectFile(t, server, "/index.html"); status != http.StatusOK || body != "v2" {
		t.Fatalf("index after replacement = %d %q", status, body)
	}
	if status, _ := getProjectFile(t, server, "/stale.txt"); status != http.StatusNotFound {
		t.Fatalf("stale file status = %d, want 404", status)
	}
}

func readTGZ(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	files, err := parseTGZ(data)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func parseTGZ(data []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := make(map[string][]byte)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[header.Name] = content
	}
	return files, nil
}

func mintProjectKey(t *testing.T, server *httptest.Server) string {
	t.Helper()
	loginBody := strings.NewReader(`{"email":"admin@example.com","password":"password123"}`)
	resp, err := server.Client().Post(server.URL+"/api/auth/login", "application/json", loginBody)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(resp.Cookies()) == 0 {
		t.Fatalf("login status = %d", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/projects/site/keys", strings.NewReader(`{"label":"cli-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(resp.Cookies()[0])
	keyResp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer keyResp.Body.Close()
	if keyResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(keyResp.Body)
		t.Fatalf("create key status = %d body=%s", keyResp.StatusCode, body)
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(keyResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return created.Token
}

func getProjectFile(t *testing.T, server *httptest.Server, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "site.localhost"
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}
