#!/usr/bin/env bash
# Build and push the two test-only images of the critical-timing experiments,
# then record the tags in images.env:
#
#   ghcr.io/braveantony/koupa-experiments:app-<commit>       the App (v1) with
#       TEST_HOOKS support, built from the committed HEAD
#   ghcr.io/braveantony/koupa-experiments:slow-board-<hash>  slow-board/
#
# The kubelet pulls them when an experiment overlay is applied, through the
# pull secret lib.sh creates while the package is private (no credential is
# ever put on a command line). The koupa images (images.env) are not touched.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
stage=$(cd "$here/../.." && pwd)
repo=ghcr.io/braveantony/koupa-experiments
cd "$stage"
# shellcheck source=../../images.env
. "$stage/images.env" # the app is the v1 line, built as KOUPA_V1_TAG
if ! git diff --quiet HEAD -- app internal go.mod go.sum; then
	echo "build-images: app/, internal/ or go.mod differ from HEAD; commit first" >&2
	exit 1
fi
app=$repo:app-$(git rev-parse --short HEAD)
sb=$repo:slow-board-$(sha256sum experiments/critical/slow-board/main.go experiments/critical/slow-board/Containerfile | sha256sum | cut -c1-12)
timeout 900 podman build -q --platform linux/amd64 -f app/Containerfile --build-arg "VERSION=$KOUPA_V1_TAG" -t "$app" .
timeout 900 podman build -q --platform linux/amd64 -f experiments/critical/slow-board/Containerfile -t "$sb" .
timeout 600 podman push -q "$app"
timeout 600 podman push -q "$sb"
cat >"$here/images.env" <<EOT
# Written by build-images.sh. Test-only images; never used by measure.sh.
CRIT_APP_IMAGE=$app
CRIT_SLOW_BOARD_IMAGE=$sb
EOT
cat "$here/images.env"
