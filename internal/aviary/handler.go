package aviary

import (
	"net/http"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/ui"
)

// buildHandler constructs the full PocketBase HTTP handler (REST API + admin
// dashboard) for an already-bootstrapped app, mirroring apis.Serve but without
// creating a TCP listener. This is what lets many apps share one process and be
// multiplexed in-memory by the Aviary front, with no per-project TCP hop.
func buildHandler(app core.App) (http.Handler, error) {
	// Apply any pending migrations, exactly as apis.Serve does on startup.
	if err := app.RunAllMigrations(); err != nil {
		return nil, err
	}

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		return nil, err
	}

	pbRouter.Bind(apis.CORS(apis.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{
			http.MethodGet, http.MethodHead, http.MethodPut,
			http.MethodPatch, http.MethodPost, http.MethodDelete,
		},
	}))

	// Serve the embedded admin dashboard SPA under /_/ when it is bundled in.
	if ui.DistDirFS != nil {
		pbRouter.GET("/_/{path...}", apis.Static(ui.DistDirFS, false)).Bind(apis.Gzip())
	}

	// Trigger the OnServe hooks so the dashboard extension routes and any
	// user-registered routes are bound, then build the std http.Handler.
	var handler http.Handler
	event := new(core.ServeEvent)
	event.App = app
	event.Router = pbRouter
	event.Server = &http.Server{}

	triggerErr := app.OnServe().Trigger(event, func(e *core.ServeEvent) error {
		mux, err := e.Router.BuildMux()
		if err != nil {
			return err
		}
		handler = mux
		return nil
	})
	if triggerErr != nil {
		return nil, triggerErr
	}

	return handler, nil
}
