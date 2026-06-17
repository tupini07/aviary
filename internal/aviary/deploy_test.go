package aviary

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
	if res.Files != 2 || res.Mode != "overlay" {
		t.Fatalf("result = %+v, want 2 files overlay", res)
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

// TestDeployOverlayVsClean verifies overlay keeps pre-existing files while clean
// replaces the directory wholesale.
func TestDeployOverlayVsClean(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// Pre-existing file uploaded separately.
	doControl(t, av, http.MethodPut, "/api/projects/alpha/files/content",
		fileContent{Path: "keep.txt", Content: "keep me"}, sess)

	// Overlay deploy: keep.txt must survive.
	doDeploy(t, av, "alpha", "", tgzArchive(t, map[string]string{"index.html": "v1"}), sess)
	if w := serveRoot(t, av, "alpha", "/keep.txt"); w.Code != http.StatusOK {
		t.Fatalf("overlay dropped keep.txt: status %d", w.Code)
	}

	// Clean deploy: keep.txt must be gone, index replaced.
	rec := doDeploy(t, av, "alpha", "?clean=true", tgzArchive(t, map[string]string{"index.html": "v2"}), sess)
	var res deployResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Mode != "replace" {
		t.Fatalf("clean mode = %q, want replace", res.Mode)
	}
	if w := serveRoot(t, av, "alpha", "/keep.txt"); w.Code == http.StatusOK {
		t.Fatalf("clean deploy kept keep.txt")
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
	rec := doDeploy(t, av, "alpha", "?clean=true", bad, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal deploy: status %d body %s", rec.Code, rec.Body.String())
	}
	// The live site must be unchanged.
	if w := serveRoot(t, av, "alpha", "/"); w.Body.String() != "original" {
		t.Fatalf("site changed after rejected deploy: %q", w.Body.String())
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
