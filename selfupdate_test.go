package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAssetName(t *testing.T) {
	cases := []struct {
		version, goos, goarch, want string
	}{
		{"0.1.0", "darwin", "arm64", "aviary_0.1.0_darwin_arm64.zip"},
		{"0.1.0", "darwin", "amd64", "aviary_0.1.0_darwin_amd64.zip"},
		{"1.2.3", "linux", "amd64", "aviary_1.2.3_linux_amd64.zip"},
		{"1.2.3", "linux", "arm", "aviary_1.2.3_linux_armv7.zip"},
		{"2.0.0", "windows", "amd64", "aviary_2.0.0_windows_amd64.zip"},
	}
	for _, c := range cases {
		if got := assetName(c.version, c.goos, c.goarch); got != c.want {
			t.Errorf("assetName(%q,%q,%q) = %q, want %q", c.version, c.goos, c.goarch, got, c.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	data := []byte("abc123  aviary_0.1.0_linux_amd64.zip\n" +
		"DEF456  aviary_0.1.0_darwin_arm64.zip\n" +
		"\n" +
		"garbage-line\n")
	m := parseChecksums(data)
	if m["aviary_0.1.0_linux_amd64.zip"] != "abc123" {
		t.Errorf("linux hash = %q", m["aviary_0.1.0_linux_amd64.zip"])
	}
	if m["aviary_0.1.0_darwin_arm64.zip"] != "def456" {
		t.Errorf("darwin hash not lowercased: %q", m["aviary_0.1.0_darwin_arm64.zip"])
	}
	if len(m) != 2 {
		t.Errorf("expected 2 entries, got %d", len(m))
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("hello aviary")
	sum := sha256.Sum256(data)
	good := hex.EncodeToString(sum[:])
	if err := verifyChecksum(data, good); err != nil {
		t.Errorf("verifyChecksum good: %v", err)
	}
	if err := verifyChecksum(data, strings.ToUpper(good)); err != nil {
		t.Errorf("verifyChecksum should be case-insensitive: %v", err)
	}
	if err := verifyChecksum(data, "deadbeef"); err == nil {
		t.Error("verifyChecksum bad: expected error")
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.2", "1.2.0", 0},
		{"1.0.0", "1.0.0-rc1", 1},  // final > pre-release
		{"1.0.0-rc1", "1.0.0", -1}, // pre-release < final
		{"1.0.0-rc2", "1.0.0-rc1", 1},
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsReleaseBuild(t *testing.T) {
	if isReleaseBuild("(untracked)") {
		t.Error("(untracked) should not be a release build")
	}
	if isReleaseBuild("") {
		t.Error("empty should not be a release build")
	}
	if !isReleaseBuild("0.1.0") {
		t.Error("0.1.0 should be a release build")
	}
}

func TestExtractBinary(t *testing.T) {
	content := []byte("#!/bin/sh\necho aviary\n")
	archive := buildZip(t, map[string][]byte{
		"aviary":  content,
		"README":  []byte("readme"),
		"LICENSE": []byte("license"),
	})
	got, err := extractBinary(archive, "linux")
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("extracted binary mismatch")
	}

	if _, err := extractBinary(archive, "windows"); err == nil {
		t.Error("expected error: no aviary.exe in archive")
	}
}

func TestReplaceExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "aviary")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := []byte("new binary content")
	if err := replaceExecutable(exe, newBin, runtime.GOOS); err != nil {
		t.Fatalf("replaceExecutable: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBin) {
		t.Errorf("binary not replaced: got %q", got)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Error("replaced binary is not executable")
	}
}

func TestRunUpdateFullFlow(t *testing.T) {
	bin := []byte("brand new aviary binary")
	archive := buildZip(t, map[string][]byte{"aviary": bin})
	srv := releaseServer(t, "v1.5.0", archive, map[string]string{
		"aviary_1.5.0_" + runtime.GOOS + "_" + archKey() + ".zip": sha256hex(archive),
	})
	defer srv.Close()

	out := &bytes.Buffer{}
	u := &updater{
		repo:           "tupini07/aviary",
		currentVersion: "1.0.0",
		goos:           runtime.GOOS,
		goarch:         runtime.GOARCH,
		apiBase:        srv.URL,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		out:            out,
	}

	// Point os.Executable's result at a temp file by replacing it directly.
	dir := t.TempDir()
	exe := filepath.Join(dir, "aviary")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// resolveExecutable uses os.Executable, which we can't easily override, so
	// exercise the download+verify+extract path and the swap separately.
	rel, err := u.fetchRelease("")
	if err != nil {
		t.Fatalf("fetchRelease: %v", err)
	}
	if rel.TagName != "v1.5.0" {
		t.Fatalf("tag = %q", rel.TagName)
	}
	name := assetName(normalizeVersion(rel.TagName), u.goos, u.goarch)
	asset, ok := findAsset(rel, name)
	if !ok {
		t.Fatalf("asset %q not found", name)
	}
	data, err := u.download(asset.URL)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	sums, _ := findAsset(rel, "checksums.txt")
	sumData, err := u.download(sums.URL)
	if err != nil {
		t.Fatalf("download checksums: %v", err)
	}
	if err := verifyChecksum(data, parseChecksums(sumData)[name]); err != nil {
		t.Fatalf("verify: %v", err)
	}
	extracted, err := extractBinary(data, u.goos)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if err := replaceExecutable(exe, extracted, u.goos); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, bin) {
		t.Errorf("final binary mismatch: %q", got)
	}
}

func TestRunUpdateCheck(t *testing.T) {
	archive := buildZip(t, map[string][]byte{"aviary": []byte("x")})
	srv := releaseServer(t, "v1.0.0", archive, map[string]string{
		"aviary_1.0.0_" + runtime.GOOS + "_" + archKey() + ".zip": sha256hex(archive),
	})
	defer srv.Close()

	out := &bytes.Buffer{}
	u := &updater{
		repo: "tupini07/aviary", currentVersion: "1.0.0",
		goos: runtime.GOOS, goarch: runtime.GOARCH,
		apiBase: srv.URL, httpClient: &http.Client{Timeout: 10 * time.Second}, out: out,
	}
	if err := u.run("", true, false); err != nil {
		t.Fatalf("run check: %v", err)
	}
	if !strings.Contains(out.String(), "latest version") {
		t.Errorf("check output missing latest: %q", out.String())
	}
	if !strings.Contains(out.String(), "latest") {
		t.Errorf("expected up-to-date message: %q", out.String())
	}
}

func TestRunUpdateRefusesUntracked(t *testing.T) {
	srv := releaseServer(t, "v1.0.0", nil, nil)
	defer srv.Close()
	out := &bytes.Buffer{}
	u := &updater{
		repo: "tupini07/aviary", currentVersion: "(untracked)",
		goos: runtime.GOOS, goarch: runtime.GOARCH,
		apiBase: srv.URL, httpClient: &http.Client{Timeout: 10 * time.Second}, out: out,
	}
	err := u.run("", false, false)
	if err == nil || !strings.Contains(err.Error(), "not a release build") {
		t.Errorf("expected refusal for untracked build, got %v", err)
	}
}

// --- helpers ---

func archKey() string {
	if runtime.GOARCH == "arm" {
		return "armv7"
	}
	return runtime.GOARCH
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func buildZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseServer stands in for the GitHub API + asset CDN. checksums maps asset
// name → hex sha256; a checksums.txt asset is synthesized from it.
func releaseServer(t *testing.T, tag string, archive []byte, checksums map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server

	var sumLines strings.Builder
	for name, h := range checksums {
		fmt.Fprintf(&sumLines, "%s  %s\n", h, name)
	}
	sumData := []byte(sumLines.String())

	release := func() ghRelease {
		assets := []ghAsset{}
		for name := range checksums {
			assets = append(assets, ghAsset{Name: name, URL: srv.URL + "/dl/" + name})
		}
		assets = append(assets, ghAsset{Name: "checksums.txt", URL: srv.URL + "/dl/checksums.txt"})
		return ghRelease{TagName: tag, Name: tag, Assets: assets}
	}

	mux.HandleFunc("/repos/tupini07/aviary/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(release())
	})
	mux.HandleFunc("/repos/tupini07/aviary/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(release())
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			w.Write(sumData)
		default:
			w.Write(archive)
		}
	})
	srv = httptest.NewServer(mux)
	return srv
}
