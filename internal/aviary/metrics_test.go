package aviary

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeFile PUTs a pb_public file via the control API and returns the recorder.
func writeFile(t *testing.T, av *Aviary, project, path, content string, sess *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return doControl(t, av, http.MethodPut, "/api/projects/"+project+"/files/content",
		fileContent{Path: path, Content: content}, sess)
}

func getMetrics(t *testing.T, av *Aviary, project string, sess *http.Cookie) projectMetrics {
	t.Helper()
	rec := doControl(t, av, http.MethodGet, "/api/projects/"+project+"/metrics", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: status %d body %s", rec.Code, rec.Body.String())
	}
	var m projectMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal metrics: %v", err)
	}
	return m
}

func TestProjectMetricsReportsPublicUsage(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// Empty project: no public files, not running, unlimited quota.
	m := getMetrics(t, av, "alpha", sess)
	if m.PublicFiles != 0 || m.PublicBytes != 0 {
		t.Fatalf("empty project: %+v", m)
	}
	if m.QuotaBytes != 0 || m.OverQuota {
		t.Fatalf("empty project quota: %+v", m)
	}
	if m.Running || m.LastActive != nil {
		t.Fatalf("empty project should not be running: %+v", m)
	}

	// Write a couple of files and confirm the byte/file accounting.
	if rec := writeFile(t, av, "alpha", "index.html", "<h1>hi</h1>", sess); rec.Code != http.StatusOK {
		t.Fatalf("write index: %d %s", rec.Code, rec.Body.String())
	}
	if rec := writeFile(t, av, "alpha", "css/app.css", "body{}", sess); rec.Code != http.StatusOK {
		t.Fatalf("write css: %d %s", rec.Code, rec.Body.String())
	}
	m = getMetrics(t, av, "alpha", sess)
	if m.PublicFiles != 2 {
		t.Fatalf("PublicFiles = %d, want 2", m.PublicFiles)
	}
	wantBytes := int64(len("<h1>hi</h1>") + len("body{}"))
	if m.PublicBytes != wantBytes {
		t.Fatalf("PublicBytes = %d, want %d", m.PublicBytes, wantBytes)
	}
	if m.StorageBytes < m.PublicBytes {
		t.Fatalf("StorageBytes %d < PublicBytes %d", m.StorageBytes, m.PublicBytes)
	}
}

func TestSetAndReportQuota(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	rec := doControl(t, av, http.MethodPatch, "/api/projects/alpha",
		patchProjectRequest{QuotaBytes: ptr[int64](100)}, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch quota: %d %s", rec.Code, rec.Body.String())
	}
	var pv projectView
	_ = json.Unmarshal(rec.Body.Bytes(), &pv)
	if pv.QuotaBytes != 100 {
		t.Fatalf("patched quota = %d, want 100", pv.QuotaBytes)
	}

	m := getMetrics(t, av, "alpha", sess)
	if m.QuotaBytes != 100 || m.OverQuota {
		t.Fatalf("metrics after quota: %+v", m)
	}
}

func TestPatchQuotaRejectsNegative(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	rec := doControl(t, av, http.MethodPatch, "/api/projects/alpha",
		patchProjectRequest{QuotaBytes: ptr[int64](-1)}, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative quota: status %d, want 400", rec.Code)
	}
}

func TestQuotaEnforcedOnFileWrite(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// Tiny quota: 20 bytes.
	if rec := doControl(t, av, http.MethodPatch, "/api/projects/alpha",
		patchProjectRequest{QuotaBytes: ptr[int64](20)}, sess); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}

	// A 10-byte file fits.
	if rec := writeFile(t, av, "alpha", "a.txt", "0123456789", sess); rec.Code != http.StatusOK {
		t.Fatalf("first write: %d %s", rec.Code, rec.Body.String())
	}
	// A second 15-byte file would push total to 25 > 20 → rejected.
	rec := writeFile(t, av, "alpha", "b.txt", "012345678901234", sess)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("over-quota write: status %d, want 507; body %s", rec.Code, rec.Body.String())
	}
	// Overwriting the existing file with the same size still fits (replacement).
	if rec := writeFile(t, av, "alpha", "a.txt", "abcdefghij", sess); rec.Code != http.StatusOK {
		t.Fatalf("overwrite same size: %d %s", rec.Code, rec.Body.String())
	}
	// Raising the quota lets the second file through.
	if rec := doControl(t, av, http.MethodPatch, "/api/projects/alpha",
		patchProjectRequest{QuotaBytes: ptr[int64](100)}, sess); rec.Code != http.StatusOK {
		t.Fatalf("raise quota: %d", rec.Code)
	}
	if rec := writeFile(t, av, "alpha", "b.txt", "012345678901234", sess); rec.Code != http.StatusOK {
		t.Fatalf("write after raise: %d %s", rec.Code, rec.Body.String())
	}
}

func TestQuotaEnforcedOnDeploy(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	if rec := doControl(t, av, http.MethodPatch, "/api/projects/alpha",
		patchProjectRequest{QuotaBytes: ptr[int64](16)}, sess); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d", rec.Code)
	}

	// Archive whose contents exceed the 16-byte quota.
	archive := tgzArchive(t, map[string]string{
		"index.html": "<h1>way too big for the quota</h1>",
	})
	rec := doDeploy(t, av, "alpha", "", archive, sess)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("over-quota deploy: status %d, want 507; body %s", rec.Code, rec.Body.String())
	}
	// The live site must be untouched (deploy rejected before swap).
	if w := serveRoot(t, av, "alpha", "/index.html"); w.Code == http.StatusOK {
		t.Fatalf("over-quota deploy should not have published; got %d", w.Code)
	}

	// A small archive within quota deploys fine.
	small := tgzArchive(t, map[string]string{"i.html": "ok"})
	if rec := doDeploy(t, av, "alpha", "", small, sess); rec.Code != http.StatusOK {
		t.Fatalf("within-quota deploy: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b"), []byte("678"), 0o644); err != nil {
		t.Fatal(err)
	}
	bytes, files, err := dirSize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bytes != 8 || files != 2 {
		t.Fatalf("dirSize = %d bytes, %d files; want 8, 2", bytes, files)
	}
	// Missing dir is treated as empty.
	b, f, err := dirSize(filepath.Join(dir, "nope"))
	if err != nil || b != 0 || f != 0 {
		t.Fatalf("missing dir: %d %d %v", b, f, err)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:          "0 B",
		512:        "512 B",
		1024:       "1.0 KiB",
		1536:       "1.5 KiB",
		1048576:    "1.0 MiB",
		1073741824: "1.0 GiB",
	}
	for n, want := range cases {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// ptr returns a pointer to v, for building optional patch fields.
func ptr[T any](v T) *T { return &v }
