package aviary

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tgzArchive builds an in-memory .tar.gz from a name→content map.
func tgzArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return buf.Bytes()
}

// zipArchive builds an in-memory .zip from a name→content map.
func zipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func symlinkArchive(t *testing.T, format string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if format == "tgz" {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{
			Name: "link", Linkname: "index.html", Mode: 0o777, Typeflag: tar.TypeSymlink,
		}); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	zw := zip.NewWriter(&buf)
	header := &zip.FileHeader{Name: "link", Method: zip.Store}
	header.SetMode(os.ModeSymlink | 0o777)
	entry, err := zw.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("index.html")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type signalingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *signalingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, io.EOF
}

// doDeploy posts a raw archive body to the deploy endpoint with a session cookie.
func doDeploy(t *testing.T, av *Aviary, project, query string, body []byte, sess *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/projects/" + project + "/deploy" + query
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/octet-stream")
	if sess != nil {
		req.AddCookie(sess)
	}
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	return rec
}

// serveRoot fetches a path at the project subdomain.
func serveRoot(t *testing.T, av *Aviary, project, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = project + ".localhost"
	w := httptest.NewRecorder()
	av.ServeHTTP(w, req)
	return w
}

func TestDeployTarGz(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	archive := tgzArchive(t, map[string]string{
		"index.html":  "<h1>deployed</h1>",
		"css/app.css": "body{color:blue}",
	})
	rec := doDeploy(t, av, "alpha", "", archive, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy: status %d body %s", rec.Code, rec.Body.String())
	}
	var res deployResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Files != 2 || res.Mode != "replace" {
		t.Fatalf("result = %+v, want 2 files replace", res)
	}

	if w := serveRoot(t, av, "alpha", "/"); w.Code != http.StatusOK || w.Body.String() != "<h1>deployed</h1>" {
		t.Fatalf("serve index: status %d body %q", w.Code, w.Body.String())
	}
	if w := serveRoot(t, av, "alpha", "/css/app.css"); w.Code != http.StatusOK || w.Body.String() != "body{color:blue}" {
		t.Fatalf("serve css: status %d body %q", w.Code, w.Body.String())
	}
}

func TestDeployZip(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	archive := zipArchive(t, map[string]string{"index.html": "<p>zip</p>"})
	rec := doDeploy(t, av, "alpha", "", archive, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy zip: status %d body %s", rec.Code, rec.Body.String())
	}
	if w := serveRoot(t, av, "alpha", "/"); w.Code != http.StatusOK || w.Body.String() != "<p>zip</p>" {
		t.Fatalf("serve zip index: status %d body %q", w.Code, w.Body.String())
	}
}

func TestDeployReplacesExistingFiles(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// Pre-existing file uploaded separately.
	doControl(t, av, http.MethodPut, "/api/projects/alpha/files/content",
		fileContent{Path: "keep.txt", Content: "keep me"}, sess)

	rec := doDeploy(t, av, "alpha", "", tgzArchive(t, map[string]string{"index.html": "v2"}), sess)
	var res deployResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Mode != "replace" {
		t.Fatalf("mode = %q, want replace", res.Mode)
	}
	if w := serveRoot(t, av, "alpha", "/keep.txt"); w.Code == http.StatusOK {
		t.Fatalf("replacement deploy kept stale keep.txt")
	}
	if w := serveRoot(t, av, "alpha", "/"); w.Body.String() != "v2" {
		t.Fatalf("clean index = %q, want v2", w.Body.String())
	}
}

// TestDeployTraversalRejectedAtomic verifies a malicious entry is rejected and
// the deploy leaves the existing site untouched (atomic staging).
func TestDeployTraversalRejectedAtomic(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	doDeploy(t, av, "alpha", "", tgzArchive(t, map[string]string{"index.html": "original"}), sess)

	bad := tgzArchive(t, map[string]string{"../escape.txt": "pwned", "index.html": "tampered"})
	rec := doDeploy(t, av, "alpha", "", bad, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal deploy: status %d body %s", rec.Code, rec.Body.String())
	}

	// The live site must be unchanged.
	if w := serveRoot(t, av, "alpha", "/"); w.Body.String() != "original" {
		t.Fatalf("site changed after rejected deploy: %q", w.Body.String())
	}
}

func TestDeployEmptyArchiveRejectedAtomic(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	doDeploy(t, av, "alpha", "", tgzArchive(t, map[string]string{"index.html": "original"}), sess)

	rec := doDeploy(t, av, "alpha", "", tgzArchive(t, nil), sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty deploy: status %d body %s", rec.Code, rec.Body.String())
	}

	if w := serveRoot(t, av, "alpha", "/"); w.Code != http.StatusOK || w.Body.String() != "original" {
		t.Fatalf("site changed after empty deploy: status %d body %q", w.Code, w.Body.String())
	}
}

func TestDeployRejectsSpecialEntriesAtomic(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	doDeploy(t, av, "alpha", "", tgzArchive(t, map[string]string{"index.html": "original"}), sess)

	for _, format := range []string{"tgz", "zip"} {
		t.Run(format, func(t *testing.T) {
			rec := doDeploy(t, av, "alpha", "", symlinkArchive(t, format), sess)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "non-regular") {
				t.Fatalf("special-entry deploy: status %d body %s", rec.Code, rec.Body.String())
			}
			if w := serveRoot(t, av, "alpha", "/"); w.Code != http.StatusOK || w.Body.String() != "original" {
				t.Fatalf("site changed after special entry: status %d body %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestSlowUploadDoesNotHoldProjectMutationLock(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	body := &signalingReader{started: make(chan struct{}), release: make(chan struct{})}
	deployReq := httptest.NewRequest(http.MethodPost, "/api/projects/alpha/deploy", body)
	deployReq.Host = "localhost"
	deployReq.AddCookie(sess)
	deployDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		av.ServeHTTP(rec, deployReq)
		deployDone <- rec
	}()
	<-body.started

	writeData, err := json.Marshal(fileContent{Path: "during-upload.txt", Content: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	writeReq := httptest.NewRequest(http.MethodPut, "/api/projects/alpha/files/content", bytes.NewReader(writeData))
	writeReq.Host = "localhost"
	writeReq.Header.Set("Content-Type", "application/json")
	writeReq.AddCookie(sess)
	writeDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		av.ServeHTTP(rec, writeReq)
		writeDone <- rec
	}()

	select {
	case rec := <-writeDone:
		if rec.Code != http.StatusOK {
			t.Fatalf("file write during upload: %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		close(body.release)
		<-deployDone
		t.Fatal("file write was blocked by a deployment still reading its request body")
	}
	close(body.release)
	if rec := <-deployDone; rec.Code != http.StatusBadRequest {
		t.Fatalf("incomplete deploy: %d %s", rec.Code, rec.Body.String())
	}
}

func TestFileWriteUsesProjectMutationLock(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	unlock, err := av.lockProjectFiles(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}

	writeData, err := json.Marshal(fileContent{Path: "locked.txt", Content: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/projects/alpha/files/content", bytes.NewReader(writeData))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sess)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		av.ServeHTTP(rec, req)
		done <- rec
	}()

	select {
	case <-done:
		unlock()
		t.Fatal("file write did not wait for the project mutation lock")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if rec := <-done; rec.Code != http.StatusOK {
		t.Fatalf("file write after unlock: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeployUnsupportedFormat(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	rec := doDeploy(t, av, "alpha", "", []byte("just some text, not an archive"), sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported body: status %d body %s", rec.Code, rec.Body.String())
	}
}

// TestDeployViaAPIKey verifies CI's intended path: deploy with a bearer key, and
// that a key for another project is rejected.
func TestDeployViaAPIKey(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "beta"}, sess)
	key := mintKey(t, av, "alpha", sess, createAPIKeyRequest{Label: "ci"})

	archive := tgzArchive(t, map[string]string{"index.html": "from ci"})

	// Bearer deploy to the bound project works.
	req := httptest.NewRequest(http.MethodPost, "/api/projects/alpha/deploy", bytes.NewReader(archive))
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+key.Token)
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer deploy: status %d body %s", rec.Code, rec.Body.String())
	}

	if w := serveRoot(t, av, "alpha", "/"); w.Body.String() != "from ci" {
		t.Fatalf("served = %q, want 'from ci'", w.Body.String())
	}

	// The same key must not deploy to another project.
	req = httptest.NewRequest(http.MethodPost, "/api/projects/beta/deploy", bytes.NewReader(archive))
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+key.Token)
	rec = httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-project deploy: status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestConcurrentDeploysAreSerialized(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	const deployments = 12
	archives := make([][]byte, deployments)
	for i := range archives {
		archives[i] = tgzArchive(t, map[string]string{"index.html": fmt.Sprintf("build-%d", i)})
	}

	results := make(chan *httptest.ResponseRecorder, deployments)
	var wg sync.WaitGroup
	for _, archive := range archives {
		wg.Add(1)
		go func(body []byte) {
			defer wg.Done()
			results <- doDeploy(t, av, "alpha", "", body, sess)
		}(archive)
	}
	wg.Wait()
	close(results)

	for rec := range results {
		if rec.Code != http.StatusOK {
			t.Fatalf("concurrent deploy: status %d body %s", rec.Code, rec.Body.String())
		}
	}
	if matches, err := filepath.Glob(filepath.Join(av.projectPath("alpha"), ".pb_public*")); err != nil {
		t.Fatal(err)
	} else if len(matches) != 0 {
		t.Fatalf("deployment left temporary directories: %v", matches)
	}
}
