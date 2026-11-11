#!/usr/bin/env bash
# Publish the koupa (and rush-hour) images; a published tag is never
# overwritten.
#
#   scripts/publish-images.sh [--dry-run]
#   scripts/publish-images.sh --retag-history [--dry-run]
#   scripts/publish-images.sh --rush-hour VERSION [--dry-run]
#
# Default mode: builds both koupa lines (KOUPA_V1_TAG, KOUPA_V2_TAG in
# images.env) from a clean export of HEAD (`git archive`, so nothing outside
# the commit gets in) with VERSION=<tag>, checks that /koupa --version
# prints the tag, pushes them and prints their manifest digests. It refuses,
# before building anything, when the working tree under 01/ is not clean,
# when a tag is not vMAJOR.MINOR.PATCH of its line (v1.x.y / v2.x.y, same
# MINOR.PATCH), or when a tag already exists on the registry or its absence
# cannot be proven (scripts/lib/registry.sh: only skopeo's exact
# "manifest unknown" for that tag counts as absent; any other error fails).
#
# --retag-history: one-off; adds the semver tags of the images published
# before images.env (table HISTORY below) to their existing digests with
# `skopeo copy --preserve-digests docker://REPO@DIGEST docker://REPO:TAG`.
# A tag already on the right digest is skipped; one on another digest stops
# the whole run before anything is copied. The legacy floating tags v1/v2
# are not touched (deprecated, frozen at v1.1.0/v2.1.0).
#
# --rush-hour VERSION: the same for ghcr.io/braveantony/rush-hour:VERSION
# (rush-hour/Containerfile, from the clean export of HEAD).
#
# --dry-run: all the checks, then prints what it would build, push or copy.
#
# Immutability is enforced by this procedure, not by the registry: ghcr has
# no atomic "create this tag only if absent". So only one publisher (the
# teacher) may publish at a time; a lock file serializes runs on one host.
# Each tag's absence is checked again right before its push or copy, and
# right after it the registry digest must equal the digest just pushed
# (podman --digestfile) or the recorded one; anything else stops loudly.
#
# Credentials: podman and skopeo read the registry login that
# `podman login ghcr.io` stored (REGISTRY_AUTH_FILE or the default
# auth.json); nothing is ever passed on a command line.
#
# Environment (tests): PODMAN, SKOPEO, PUBLISH_STAGE (the 01/ directory),
# PUBLISH_LOCK (lock file).
# Exit: 0 done; 1 refused or failed; 64 usage.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
stage=${PUBLISH_STAGE:-$(cd "$here/.." && pwd)}
PODMAN=${PODMAN:-podman}
SKOPEO=${SKOPEO:-skopeo}
export SKOPEO
PLATFORM=linux/amd64
RUSH_HOUR_REPO=ghcr.io/braveantony/rush-hour
# shellcheck source=lib/registry.sh
. "$here/lib/registry.sh"

# tag  manifest digest  (01/docs/design-v2/06-reference.md section 6.2)
HISTORY=(
	"v1.0.0 sha256:e34801f1457f6026984c3b9095cd32eecb91f158d32b8475e5374a554ccd9fb3"
	"v2.0.0 sha256:935764966ab0f6d68e1b292bf18befcdf33310ceed073d17fa77a43ded69b891"
	"v1.1.0 sha256:e767bd6d12d316733c826da07b387674791c4f7e1e11cff8d5f1b8aa0459bdea"
	"v2.1.0 sha256:082a7412ec4de56f160c7d1bbe91162a7f119a4e0742421345d866c609648cf4"
)

usage() {
	sed -n '5,7p' "$0" | sed 's/^# \{0,1\}/usage: /' >&2
	exit 64
}
say() { printf 'publish-images: %s\n' "$*"; }
die() {
	printf 'publish-images: %s\n' "$*" >&2
	exit 1
}

mode=publish dry_run=0 rh_version=""
while [ $# -gt 0 ]; do
	case $1 in
	--retag-history) mode=retag; shift ;;
	--rush-hour) [ $# -ge 2 ] || usage; mode=rush-hour rh_version=$2; shift 2 ;;
	--dry-run) dry_run=1; shift ;;
	*) usage ;;
	esac
done

[ -f "$stage/images.env" ] || die "$stage/images.env not found"
# shellcheck source=../images.env
. "$stage/images.env"
repo=${KOUPA_REPO:?images.env: KOUPA_REPO is not set}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# One publisher at a time on this host (the registry cannot enforce it).
lock=${PUBLISH_LOCK:-${XDG_RUNTIME_DIR:-/tmp}/koupa-publish-images.lock}
exec 9>"$lock"
flock -n 9 || die "another publish-images.sh is running (lock $lock)"

# digest_of REF: the registry digest or "absent"; dies when it cannot tell.
digest_of() {
	registry_digest "$1" "$tmp/inspect.err" || die "refusing: $(cat "$tmp/inspect.err")"
}

# require_absent REF...: refuses when any REF exists (all are checked).
require_absent() {
	local r d bad=()
	for r in "$@"; do
		d=$(digest_of "$r") || exit 1
		[ "$d" = absent ] || bad+=("$r ($d)")
	done
	[ ${#bad[@]} -eq 0 ] ||
		die "refusing: already published, tags are never overwritten: ${bad[*]}; bump the version"
}

require_login() {
	local host=${repo%%/*}
	"$PODMAN" login --get-login "$host" >/dev/null 2>&1 ||
		die "not logged in to $host: run podman login $host first (the token is read from there, never from argv)"
}

# require_clean_head: sets commit, prefix; refuses a dirty tree under 01/.
require_clean_head() {
	git -C "$stage" rev-parse --verify -q HEAD >/dev/null || die "$stage is not in a git repository with a commit"
	local dirty
	dirty=$(git -C "$stage" status --porcelain --untracked-files=normal -- . 2>&1) || die "git status failed: $dirty"
	[ -z "$dirty" ] || die "refusing: the working tree under $stage is not clean; commit first:
$dirty"
	commit=$(git -C "$stage" rev-parse HEAD)
	prefix=$(git -C "$stage" rev-parse --show-prefix)
}

export_head() {
	ctx=$tmp/src
	mkdir -p "$ctx"
	git -C "$stage" archive --format=tar "HEAD:${prefix}" | tar -x -C "$ctx" || die "git archive of HEAD failed"
}

# push_new IMAGE: re-check absence, push, verify the registry digest equals
# the digest podman just pushed.
push_new() {
	local img=$1 pushed got
	require_absent "$img"
	rm -f "$tmp/digest"
	"$PODMAN" push --digestfile "$tmp/digest" "$img" || die "push of $img failed"
	pushed=$(cat "$tmp/digest" 2>/dev/null || true)
	[[ $pushed =~ ^sha256:[0-9a-f]{64}$ ]] || die "push of $img reported no digest ($pushed)"
	got=$(digest_of "$img") || exit 1
	[ "$got" = "$pushed" ] ||
		die "ALERT: $img is $got on the registry right after pushing $pushed: another publisher wrote it? Stop and check before anyone pulls it"
	say "pushed $img $got"
}

# ------------------------------------------------------------ retag-history
if [ "$mode" = retag ]; then
	todo=()
	for row in "${HISTORY[@]}"; do
		read -r tag digest <<<"$row"
		src=$(digest_of "$repo@$digest") || exit 1
		[ "$src" = "$digest" ] || die "refusing: $repo@$digest is not on the registry ($src)"
		have=$(digest_of "$repo:$tag") || exit 1
		if [ "$have" = absent ]; then
			todo+=("$row")
		elif [ "$have" = "$digest" ]; then
			say "$repo:$tag is already $digest: skipped"
		else
			die "refusing: $repo:$tag exists with $have, not $digest; tags are never overwritten (nothing copied)"
		fi
	done
	if [ ${#todo[@]} -eq 0 ]; then
		say "nothing to do"
		exit 0
	fi
	if [ "$dry_run" = 1 ]; then
		for row in "${todo[@]}"; do
			read -r tag digest <<<"$row"
			say "dry run: would copy $repo@$digest to $repo:$tag"
		done
		exit 0
	fi
	require_login
	for row in "${todo[@]}"; do
		read -r tag digest <<<"$row"
		require_absent "$repo:$tag" # again, right before the copy
		"$SKOPEO" copy --preserve-digests "docker://$repo@$digest" "docker://$repo:$tag" >"$tmp/copy.log" 2>&1 ||
			die "skopeo copy to $repo:$tag failed: $(tail -c 300 "$tmp/copy.log" | tr '\n' ' ')"
		got=$(digest_of "$repo:$tag") || exit 1
		[ "$got" = "$digest" ] ||
			die "ALERT: $repo:$tag is $got right after the copy, want $digest: another publisher wrote it?"
		say "tagged $repo:$tag $got"
	done
	exit 0
fi

# ---------------------------------------------------------------- rush-hour
if [ "$mode" = rush-hour ]; then
	[[ $rh_version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
		die "refusing: rush-hour version $rh_version is not MAJOR.MINOR.PATCH"
	img=$RUSH_HOUR_REPO:$rh_version
	require_clean_head
	require_absent "$img"
	if [ "$dry_run" = 1 ]; then
		say "dry run: would build $img from $commit and push it"
		exit 0
	fi
	require_login
	export_head
	say "building $img from $commit"
	"$PODMAN" build --platform "$PLATFORM" -f "$ctx/rush-hour/Containerfile" \
		--label "org.opencontainers.image.version=$rh_version" \
		--label "org.opencontainers.image.revision=$commit" \
		-t "$img" "$ctx" || die "build of $img failed"
	push_new "$img"
	say "done: bump RUSH_HOUR_VERSION / RUSH_HOUR_IMAGE and record the digest (TEACHER.md section 10)"
	exit 0
fi

# ------------------------------------------------------------------ publish
semver='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
[[ ${KOUPA_V1_TAG:-} =~ ^v1\.$semver$ ]] || die "refusing: KOUPA_V1_TAG=${KOUPA_V1_TAG:-} is not v1.MINOR.PATCH"
[[ ${KOUPA_V2_TAG:-} =~ ^v2\.$semver$ ]] || die "refusing: KOUPA_V2_TAG=${KOUPA_V2_TAG:-} is not v2.MINOR.PATCH"
[ "${KOUPA_V1_TAG#v1.}" = "${KOUPA_V2_TAG#v2.}" ] ||
	die "refusing: KOUPA_V1_TAG=$KOUPA_V1_TAG and KOUPA_V2_TAG=$KOUPA_V2_TAG differ in MINOR.PATCH (both lines move together)"

require_clean_head
require_absent "$repo:$KOUPA_V1_TAG" "$repo:$KOUPA_V2_TAG"

if [ "$dry_run" = 1 ]; then
	say "dry run: would build $repo:$KOUPA_V1_TAG and $repo:$KOUPA_V2_TAG from $commit and push them"
	exit 0
fi
require_login
export_head

for tag in "$KOUPA_V1_TAG" "$KOUPA_V2_TAG"; do
	img=$repo:$tag
	say "building $img from $commit"
	"$PODMAN" build --platform "$PLATFORM" -f "$ctx/app/Containerfile" \
		--build-arg "VERSION=$tag" \
		--label "org.opencontainers.image.version=$tag" \
		--label "org.opencontainers.image.revision=$commit" \
		-t "$img" "$ctx" || die "build of $img failed"
	v=$("$PODMAN" run --rm --platform "$PLATFORM" "$img" --version 2>&1) || die "$img --version failed: $v"
	[ "$v" = "$tag" ] || die "$img reports version $v, want $tag"
done

for tag in "$KOUPA_V1_TAG" "$KOUPA_V2_TAG"; do
	push_new "$repo:$tag"
done
say "done: record the digests in 01/docs/design-v2/06-reference.md section 6.2, then announce the release"
