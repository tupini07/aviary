package aviary

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Deploy limits guard against oversized or malicious archives (e.g. zip bombs).
const (
	// maxDeployUpload caps the compressed request body.
	maxDeployUpload = 50 << 20 // 50 MiB
	// maxDeployTotalSize caps the total uncompressed bytes written.
	maxDeployTotalSize = 250 << 20 // 250 MiB
	// maxDeployFiles caps the number of files extracted from one archive.
	maxDeployFiles = 5000
)

// deployResult summarizes a successful deploy.
type deployResult struct {
	Mode  string `json:"mode"` // "replace" or "overlay"
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// apiDeployProject accepts a .tar.gz or .zip archive of a built site and
// publishes it into the project's pb_public directory in a single atomic swap,
// so the project is never left half-deployed. By default the archive is overlaid
// on top of any existing files; with ?clean=true the directory is replaced
// wholesale. Authorized like the file endpoints (superuser, granted
// collaborator, or a project-scoped API key), making it the deploy target for
// agents and CI.
func (a *Aviary) apiDeployProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAccess(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}

	clean := r.URL.Query().Get("clean") == "true"

	r.Body = http.MaxBytesReader(w, r.Body, maxDeployUpload)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		a.apiError(w, http.StatusRequestEntityTooLarge, "archive exceeds the upload limit")
		return
	}
	format := detectArchiveFormat(data)
	if format == "" {
		a.apiError(w, http.StatusBadRequest, "unsupported archive: expected a .tar.gz (gzip) or .zip body")
		return
	}

	projectDir := filepath.Join(a.projectsDir, id)
	publicDir := a.projectPublicDir(id)

	staging, err := os.MkdirTemp(projectDir, ".pb_public_staging_")
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "cannot stage deploy: "+err.Error())
		return
	}
	// Best-effort cleanup; after a successful swap the staging path no longer
	// exists (it was renamed into place), so RemoveAll is a harmless no-op.
	defer func() { _ = os.RemoveAll(staging) }()

	if !clean {
		if err := copyTree(publicDir, staging); err != nil {
			a.apiError(w, http.StatusInternalServerError, "cannot seed overlay: "+err.Error())
			return
		}
	}

	count, total, err := extractArchive(staging, data, format)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "invalid archive: "+err.Error())
		return
	}

	// Enforce the project's storage quota against the fully-staged tree (which,
	// for an overlay, includes the retained existing files) before swapping.
	if p, perr := a.store.Get(r.Context(), id); perr == nil && p.QuotaBytes > 0 {
		staged, _, derr := dirSize(staging)
		if derr == nil && !withinQuota(p.QuotaBytes, staged) {
			a.apiError(w, http.StatusInsufficientStorage,
				"storage quota exceeded: this deploy would use "+formatBytes(staged)+
					" of the "+formatBytes(p.QuotaBytes)+" quota")
			return
		}
	}

	if err := swapDir(publicDir, staging); err != nil {
		a.apiError(w, http.StatusInternalServerError, "cannot publish deploy: "+err.Error())
		return
	}

	mode := "overlay"
	if clean {
		mode = "replace"
	}
	a.log.Info("deploy published", "project", id, "mode", mode, "files", count, "bytes", total)
	writeJSON(w, http.StatusOK, deployResult{Mode: mode, Files: count, Bytes: total})
}

// detectArchiveFormat sniffs an archive's magic bytes, returning "tgz", "zip" or
// "" for an unrecognized body.
func detectArchiveFormat(data []byte) string {
	switch {
	case len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b:
		return "tgz"
	case len(data) >= 4 && data[0] == 'P' && data[1] == 'K' && data[2] == 0x03 && data[3] == 0x04:
		return "zip"
	default:
		return ""
	}
}

// extractArchive writes every regular file in the archive into root, enforcing
// path-safety and the deploy size/count caps. It returns the number of files and
// total bytes written.
func extractArchive(root string, data []byte, format string) (int, int64, error) {
	if format == "zip" {
		return extractZip(root, data)
	}
	return extractTarGz(root, data)
}

func extractTarGz(root string, data []byte) (int, int64, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var count int
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, 0, fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // skip dirs, symlinks, devices, etc.
		}
		n, err := writeArchiveFile(root, hdr.Name, tr, &count, total)
		if err != nil {
			return 0, 0, err
		}
		total += n
	}
	return count, total, nil
}

func extractZip(root string, data []byte) (int, int64, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, 0, fmt.Errorf("zip: %w", err)
	}
	var count int
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return 0, 0, fmt.Errorf("zip open %q: %w", f.Name, err)
		}
		n, err := writeArchiveFile(root, f.Name, rc, &count, total)
		rc.Close()
		if err != nil {
			return 0, 0, err
		}
		total += n
	}
	return count, total, nil
}

// writeArchiveFile validates name, enforces the deploy caps and writes one file
// from src into root, returning the bytes written.
func writeArchiveFile(root, name string, src io.Reader, count *int, written int64) (int64, error) {
	if *count+1 > maxDeployFiles {
		return 0, fmt.Errorf("too many files (limit %d)", maxDeployFiles)
	}
	dest, err := safeArchivePath(root, name)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Bound the copy so a lying header or zip bomb cannot exhaust the disk.
	budget := maxDeployTotalSize - written
	if budget < 0 {
		budget = 0
	}
	n, err := io.CopyN(f, src, budget+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if n > budget {
		return 0, fmt.Errorf("archive exceeds the %d MiB total size limit", maxDeployTotalSize>>20)
	}
	*count++
	return n, nil
}

// safeArchivePath resolves an archive entry name against root, rejecting any
// path that would escape it (absolute paths, "..", etc.).
func safeArchivePath(root, name string) (string, error) {
	name = strings.TrimSpace(filepath.ToSlash(name))
	name = strings.TrimPrefix(name, "./")
	if name == "" || name == "." {
		return "", errors.New("empty entry path")
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", fmt.Errorf("unsafe path %q", name)
		}
	}
	cleaned := filepath.Clean("/" + filepath.FromSlash(name))
	full := filepath.Join(root, cleaned)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path %q", name)
	}
	if rel == "." {
		return "", fmt.Errorf("entry is not a file: %q", name)
	}
	return full, nil
}

// copyTree recursively copies the regular files under src into dst. A missing
// src is treated as empty (nothing to copy).
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// swapDir atomically replaces publicDir with the already-populated staging
// directory: the current directory is renamed aside, staging is moved into
// place, then the old copy is removed. On failure the original is restored.
// staging and publicDir must share a parent (same filesystem) so the renames
// are atomic.
func swapDir(publicDir, staging string) error {
	if err := os.Chmod(staging, 0o755); err != nil {
		return err
	}
	parent := filepath.Dir(publicDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	var backup string
	if _, err := os.Stat(publicDir); err == nil {
		backup = filepath.Join(parent, "."+filepath.Base(publicDir)+".old."+randSuffix())
		if err := os.Rename(publicDir, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.Rename(staging, publicDir); err != nil {
		if backup != "" {
			_ = os.Rename(backup, publicDir) // best-effort rollback
		}
		return err
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func randSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
