package aviary

import (
	"net/http"
	"strings"
)

// apiLlmsTxt serves an /llms.txt index (https://llmstxt.org) for the control
// plane. It is deliberately a thin, link-first map: a short block of stable
// operating-model invariants plus links to the canonical, always-current
// sources (the generated OpenAPI spec, the README, the PocketBase JS docs) so
// nothing voluminous is duplicated here and left to drift.
func (a *Aviary) apiLlmsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(controlLlmsTxt(requestBaseURL(r))))
}

// controlLlmsTxt renders the /llms.txt body. base is the absolute origin the
// caller reached Aviary on, so same-host links resolve regardless of the
// deployment's domain.
func controlLlmsTxt(base string) string {
	base = strings.TrimRight(base, "/")
	return `# Aviary

> Aviary is a single-operator control plane that hosts many isolated PocketBase
> backends ("cages") in one process, routed by subdomain, with lazy boot and
> idle eviction. One operator runs many projects; each project is a full
> PocketBase instance.

This file orients LLMs and agents working with an Aviary deployment. It links to
canonical, always-current sources instead of restating them — prefer those over
any cached summary.

## Operating model (stable invariants)

- Each project is reached at its own subdomain: ` + "`<project>.<aviary-host>`" + `.
- Projects are lazily booted on first request and evicted when idle, so work
  that must run on a schedule belongs in a control-plane cron job, not an
  in-project ` + "`cronAdd()`" + ` (which dies with the cage).
- Backend logic is added as PocketBase JS hooks in a project's ` + "`pb_hooks`" + `
  directory (` + "`*.pb.js`" + ` files). Saving a hook reboots that project.
- Cron jobs are owned by the control plane: a schedule points at a ` + "`POST`" + ` route
  (by convention ` + "`/cron/…`" + `) that the control plane invokes with a freshly
  minted superuser token, waking the project on demand.
- Auth: an operator session (superuser or granted collaborator) can do
  everything; a project-scoped API key can manage that project's files, keys and
  metrics but NOT server-side code (hooks and crons are owner-only).

## API

- [Control-plane API (OpenAPI)](` + base + `/api/openapi.json): the complete, live
  description of every control-plane endpoint. Always prefer this over a
  hand-written endpoint list.
- Each project also serves its own PocketBase API spec at
  ` + "`<project>.<aviary-host>/__aviary/openapi.json`" + `.

## Docs

- [README](https://github.com/tupini07/aviary#readme): concepts and operations —
  cages, idle eviction, JS hooks, scheduled jobs, storage quotas, deployment.
- [PocketBase JS hooks](https://pocketbase.io/docs/js-overview/): the API
  available inside ` + "`pb_hooks`" + ` (globals like ` + "`$app`" + `, ` + "`routerAdd`" + `, ` + "`cronAdd`" + `,
  event hooks). Aviary registers the stock jsvm plugin unmodified, so all
  PocketBase JS globals are available.
`
}
