#!/usr/bin/env bash
# Build and push the two test-only images of the critical-timing experiments
# and pre-pull them on every worker, then record the tags in images.env:
#
#   ghcr.io/braveantony/koupa-experiments:app-<commit>       the App (v1) with
#       TEST_HOOKS support, built from the committed HEAD
#   ghcr.io/braveantony/koupa-experiments:slow-board-<hash>  slow-board/
#
# The package is private on ghcr, so the workers pull with the host's
# podman credentials (crictl pull --auth, never stored in the cluster).
# koupa:v1 / :v2 and the workers' copies of them are not touched.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
stage=$(cd "$here/../.." && pwd)
repo=ghcr.io/braveantony/koupa-experiments
cd "$stage"
if ! git diff --quiet HEAD -- app internal go.mod go.sum; then
	echo "build-images: app/, internal/ or go.mod differ from HEAD; commit first" >&2
	exit 1
fi
app=$repo:app-$(git rev-parse --short HEAD)
sb=$repo:slow-board-$(sha256sum experiments/critical/slow-board/main.go experiments/critical/slow-board/Containerfile | sha256sum | cut -c1-12)
timeout 900 podman build -q --platform linux/amd64 -f app/Containerfile --build-arg VERSION=v1 -t "$app" .
timeout 900 podman build -q --platform linux/amd64 -f experiments/critical/slow-board/Containerfile -t "$sb" .
timeout 600 podman push -q "$app"
timeout 600 podman push -q "$sb"
auth=$(python3 -I -c 'import json,os,sys; print(json.load(open(sys.argv[1]))["auths"]["ghcr.io"]["auth"])' \
	"${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/containers/auth.json")
for n in $(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o name | sed 's#node/##'); do
	for img in "$app" "$sb"; do
		timeout 300 sudo podman exec "$n" crictl pull --auth "$auth" "$img" >/dev/null
		echo "pulled $img on $n"
	done
done
cat >"$here/images.env" <<EOT
# Written by build-images.sh. Test-only images; never used by measure.sh.
CRIT_APP_IMAGE=$app
CRIT_SLOW_BOARD_IMAGE=$sb
EOT
cat "$here/images.env"
