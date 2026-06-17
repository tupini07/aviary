#!/usr/bin/env sh
# Deploy a built web app (a directory of static files) to an Aviary project.
#
# Aviary serves each project's pb_public/ at its public origin. This script
# tars the *contents* of a build directory and uploads them to the project's
# atomic deploy endpoint, which swaps them into place with no half-deployed
# state and no server reboot.
#
# Usage:
#   AVIARY_URL=https://console.example.com \
#   AVIARY_PROJECT=little-green-notebook \
#   AVIARY_KEY=av_xxx \
#   ./deploy.sh ./dist [--clean]
#
# Environment:
#   AVIARY_URL      Base URL of the Aviary control plane (no trailing slash).
#   AVIARY_PROJECT  Project id to deploy into.
#   AVIARY_KEY      A project-scoped API key (av_...). Create one in the Files
#                   view of the control plane, or via POST /api/projects/{id}/keys.
#
# Arguments:
#   $1              Directory whose *contents* become pb_public/ (default: dist).
#   --clean         Replace pb_public entirely instead of overlaying (default
#                   is overlay, which keeps files not present in the archive).
set -eu

DIST="${1:-dist}"
QUERY=""
if [ "${2:-}" = "--clean" ]; then
	QUERY="?clean=true"
fi

: "${AVIARY_URL:?set AVIARY_URL to the control-plane base URL}"
: "${AVIARY_PROJECT:?set AVIARY_PROJECT to the project id}"
: "${AVIARY_KEY:?set AVIARY_KEY to a project API key}"

if [ ! -d "$DIST" ]; then
	echo "deploy.sh: build directory '$DIST' does not exist" >&2
	exit 1
fi

echo "Deploying contents of '$DIST' to project '$AVIARY_PROJECT'..."

# Tar the *contents* of $DIST (-C $DIST .) so paths are relative to pb_public.
tar -C "$DIST" -czf - . | curl --fail --show-error --silent \
	-X POST \
	-H "Authorization: Bearer $AVIARY_KEY" \
	-H "Content-Type: application/gzip" \
	--data-binary @- \
	"$AVIARY_URL/api/projects/$AVIARY_PROJECT/deploy$QUERY"

echo
echo "Done."
