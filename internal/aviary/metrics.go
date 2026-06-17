package aviary

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// projectMetrics summarizes a project's storage usage and live runtime state.
type projectMetrics struct {
	Running       bool       `json:"running"`
	LastActive    *time.Time `json:"lastActive"`
	StorageBytes  int64      `json:"storageBytes"`  // whole data dir (db + public + internal)
	PublicBytes   int64      `json:"publicBytes"`   // pb_public static files
	DatabaseBytes int64      `json:"databaseBytes"` // storage minus public (PocketBase dbs, logs, hooks)
	PublicFiles   int        `json:"publicFiles"`   // regular files under pb_public
	QuotaBytes    int64      `json:"quotaBytes"`    // pb_public quota; 0 = unlimited
	OverQuota     bool       `json:"overQuota"`     // PublicBytes exceeds a non-zero quota
}

// apiProjectMetrics reports a project's storage usage (total, pb_public and the
// remaining PocketBase data) plus its quota and whether the cage is booted.
// Authorized like the file endpoints (superuser, granted collaborator, or a
// project-scoped API key).
func (a *Aviary) apiProjectMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAccess(w, r, id); !ok {
		return
	}
	p, err := a.store.Get(r.Context(), id)
	switch {
	case err == ErrNotFound:
		a.apiError(w, http.StatusNotFound, "project not found")
		return
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	total, _, err := dirSize(a.projectPath(id))
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	public, files, err := dirSize(a.projectPublicDir(id))
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dataBytes := total - public
	if dataBytes < 0 {
		dataBytes = 0
	}

	m := projectMetrics{
		Running:       a.runningProjects()[id],
		StorageBytes:  total,
		PublicBytes:   public,
		DatabaseBytes: dataBytes,
		PublicFiles:   files,
		QuotaBytes:    p.QuotaBytes,
		OverQuota:     p.QuotaBytes > 0 && public > p.QuotaBytes,
	}
	if last, ok := a.lastActive(id); ok {
		m.LastActive = &last
	}
	writeJSON(w, http.StatusOK, m)
}

// lastActive returns when the project's cage was last used, if it is currently
// booted. It reports ok=false when the project is not running.
func (a *Aviary) lastActive(id string) (time.Time, bool) {
	a.mu.Lock()
	c, ok := a.cages[id]
	a.mu.Unlock()
	if !ok || !c.isReady() {
		return time.Time{}, false
	}
	return c.lastUsed(), true
}

// dirSize returns the total bytes and number of regular files under root. A
// missing root is treated as empty (0, 0, nil).
func dirSize(root string) (bytes int64, files int, err error) {
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil // directory not created yet
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		bytes += info.Size()
		files++
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return 0, 0, walkErr
	}
	return bytes, files, nil
}

// withinQuota reports whether a project's pb_public may grow to projected bytes.
// A non-positive quota means unlimited. quota is the project's configured limit.
func withinQuota(quota, projected int64) bool {
	return quota <= 0 || projected <= quota
}

// checkWriteQuota verifies that writing newSize bytes to the file at full
// (within project id's pb_public) would not exceed the project's quota. It
// accounts for replacing an existing file of the same name. It returns a
// human-readable message and ok=false when the write must be rejected.
func (a *Aviary) checkWriteQuota(r *http.Request, id, full string, newSize int64) (string, bool) {
	p, err := a.store.Get(r.Context(), id)
	if err != nil || p.QuotaBytes <= 0 {
		return "", true // unknown project handled elsewhere; 0 = unlimited
	}
	current, _, err := dirSize(a.projectPublicDir(id))
	if err != nil {
		return "", true // don't block writes on a transient stat error
	}
	var existing int64
	if info, statErr := os.Stat(full); statErr == nil && info.Mode().IsRegular() {
		existing = info.Size()
	}
	projected := current - existing + newSize
	if !withinQuota(p.QuotaBytes, projected) {
		return "storage quota exceeded: this write would use " +
			formatBytes(projected) + " of the " + formatBytes(p.QuotaBytes) + " quota", false
	}
	return "", true
}

// formatBytes renders a byte count in a compact, human-readable form.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
