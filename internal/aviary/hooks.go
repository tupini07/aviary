package aviary

import (
	"net/http"
)

// The hooks editor exposes a project's pb_hooks directory through the same
// list/read/write/delete surface as the pb_public file editor, with three
// deliberate differences:
//
//   - Authorization is owner-only (authorizeProjectAdmin): hook files are
//     executed server-side, so a leaked project-scoped API key must never be
//     able to write them.
//   - Writes are exempt from the pb_public storage quota; hook files are small
//     code files, not user-facing static assets.
//   - A successful write or delete evicts the cage so it reboots and re-runs the
//     hooks, making edits take effect without a manual restart.

// apiListHooks returns a flat, sorted listing of every file under a project's
// pb_hooks directory.
func (a *Aviary) apiListHooks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}
	a.listFilesInRoot(w, a.projectHooksDir(id))
}

// apiReadHook returns the text content of a single pb_hooks file.
func (a *Aviary) apiReadHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}
	a.readFileFromRoot(w, a.projectHooksDir(id), r.URL.Query().Get("path"))
}

// apiWriteHook creates or overwrites a pb_hooks file, then evicts the cage so
// the project reboots with the new hooks on its next request.
func (a *Aviary) apiWriteHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}
	if a.writeFileToRoot(w, r, id, a.projectHooksDir(id), false) {
		a.evict(id)
	}
}

// apiDeleteHook removes a single pb_hooks file, then evicts the cage so the
// project reboots without the removed hook on its next request.
func (a *Aviary) apiDeleteHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}
	if a.deleteFileFromRoot(w, a.projectHooksDir(id), r.URL.Query().Get("path")) {
		a.evict(id)
	}
}
