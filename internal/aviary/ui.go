package aviary

import (
	_ "embed"
	"net/http"
)

//go:embed web/index.html
var controlUI []byte

// landing serves the control-plane web UI. The single-page app drives all
// project and superuser management through the JSON API; everything it needs
// (auth state, project list) is fetched client-side.
func (a *Aviary) landing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(controlUI)
}
