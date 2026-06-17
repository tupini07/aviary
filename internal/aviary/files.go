package aviary

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxEditableFileSize caps the size of files the static editor will read or
// write, keeping the textarea-based editor responsive and bounding memory use.
const maxEditableFileSize = 5 << 20 // 5 MiB

// authorizeProjectAccess verifies the caller may administer project id: a
// superuser may touch any project, a collaborator only the projects they have
// been granted. It writes the appropriate error response and returns false on
// denial.
func (a *Aviary) authorizeProjectAccess(w http.ResponseWriter, r *http.Request, id string) (email string, ok bool) {
	email, role, authed := a.identity(r)
	if !authed {
		a.apiError(w, http.StatusUnauthorized, "authentication required")
		return "", false
	}
	if role != roleSuperuser {
		granted, err := a.store.CollaboratorHasProject(r.Context(), email, id)
		if err != nil {
			a.apiError(w, http.StatusInternalServerError, err.Error())
			return "", false
		}
		if !granted {
			a.apiError(w, http.StatusForbidden, "no access to this project")
			return "", false
		}
	}
	return email, true
}

// projectPublicDir returns the absolute path of a project's pb_public directory,
// the folder whose contents are served at the project's public URL.
func (a *Aviary) projectPublicDir(id string) string {
	return filepath.Join(a.projectsDir, id, "pb_public")
}

// resolvePublicPath joins a caller-supplied relative path onto a project's
// pb_public directory, rejecting anything that would escape it (via "..",
// absolute paths, etc.). The returned path is guaranteed to live inside root.
func resolvePublicPath(root, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("path is required")
	}
	// Reject any explicit parent-directory segment outright, so traversal
	// attempts fail loudly instead of being silently clamped to the root.
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return "", errors.New("invalid path")
		}
	}
	// Anchoring at "/" then cleaning collapses any leading "../" so the result
	// can never climb above the root once re-joined.
	cleaned := filepath.Clean("/" + filepath.FromSlash(rel))
	full := filepath.Join(root, cleaned)
	within, err := filepath.Rel(root, full)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid path")
	}
	if within == "." {
		return "", errors.New("path must reference a file, not the root")
	}
	return full, nil
}

type fileEntry struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

// apiListFiles returns a flat, sorted listing of every regular file under a
// project's pb_public directory.
func (a *Aviary) apiListFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAccess(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}

	root := a.projectPublicDir(id)
	entries := make([]fileEntry, 0)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil // pb_public not created yet -> empty listing
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entries = append(entries, fileEntry{
			Path:     filepath.ToSlash(rel),
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
		})
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		a.apiError(w, http.StatusInternalServerError, walkErr.Error())
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	writeJSON(w, http.StatusOK, entries)
}

type fileContent struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// apiReadFile returns the text content of a single pb_public file.
func (a *Aviary) apiReadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAccess(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}

	full, err := resolvePublicPath(a.projectPublicDir(id), r.URL.Query().Get("path"))
	if err != nil {
		a.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := os.Stat(full)
	switch {
	case os.IsNotExist(err):
		a.apiError(w, http.StatusNotFound, "file not found")
		return
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	case info.IsDir():
		a.apiError(w, http.StatusBadRequest, "path is a directory")
		return
	case info.Size() > maxEditableFileSize:
		a.apiError(w, http.StatusRequestEntityTooLarge, "file is too large to edit")
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rel, _ := filepath.Rel(a.projectPublicDir(id), full)
	writeJSON(w, http.StatusOK, fileContent{Path: filepath.ToSlash(rel), Content: string(data)})
}

// apiWriteFile creates or overwrites a pb_public file with the supplied text,
// creating any missing parent directories.
func (a *Aviary) apiWriteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAccess(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}

	var req fileContent
	if !decodeJSON(w, r, &req, a) {
		return
	}
	if len(req.Content) > maxEditableFileSize {
		a.apiError(w, http.StatusRequestEntityTooLarge, "content is too large")
		return
	}

	full, err := resolvePublicPath(a.projectPublicDir(id), req.Path)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		a.apiError(w, http.StatusBadRequest, "cannot create parent directory: "+err.Error())
		return
	}
	if err := os.WriteFile(full, []byte(req.Content), 0o644); err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rel, _ := filepath.Rel(a.projectPublicDir(id), full)
	writeJSON(w, http.StatusOK, fileEntry{
		Path: filepath.ToSlash(rel),
		Size: int64(len(req.Content)),
	})
}

// apiDeleteFile removes a single pb_public file.
func (a *Aviary) apiDeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAccess(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}

	full, err := resolvePublicPath(a.projectPublicDir(id), r.URL.Query().Get("path"))
	if err != nil {
		a.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := os.Stat(full)
	switch {
	case os.IsNotExist(err):
		a.apiError(w, http.StatusNotFound, "file not found")
		return
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	case info.IsDir():
		a.apiError(w, http.StatusBadRequest, "refusing to delete a directory")
		return
	}
	if err := os.Remove(full); err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// projectExists writes a 404 and returns false when project id is not
// provisioned. It is a lightweight guard shared by the file endpoints.
func (a *Aviary) projectExists(w http.ResponseWriter, r *http.Request, id string) bool {
	_, err := a.store.Get(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		a.apiError(w, http.StatusNotFound, "project not found")
		return false
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	return true
}
