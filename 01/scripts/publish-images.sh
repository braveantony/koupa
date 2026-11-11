#!/usr/bin/env bash
# Publish the koupa images with the versions in images.env; a published tag
# is never overwritten.
#
#   scripts/publish-images.sh [--dry-run]
#   scripts/publish-images.sh --retag-history [--dry-run]
#
# Default mode: builds both lines (KOUPA_V1_TAG, KOUPA_V2_TAG) from a clean
# export of HEAD (`git archive`, so nothing outside the commit gets in) with
# VERSION=<tag>, checks that /koupa --version prints the tag, pushes them
# and prints their manifest digests. It refuses, before building anything,
# when the working tree under 01/ is not clean, when a tag is not
# vMAJOR.MINOR.PATCH of its line (v1.x.y / v2.x.y, same MINOR.PATCH), or when
# a tag already exists on the registry (or its absence cannot be shown).
#
# --retag-history: one-off; adds the semver tags of the images published
# before images.env (table HISTORY below) to their existing digests with
# `skopeo copy --preserve-digests docker://REPO@DIGEST docker://REPO:TAG`.
# A tag already on the right digest is skipped; one on another digest stops
# the whole run before anything is copied. The legacy floating tags v1/v2
# are not touched (deprecated, frozen at v1.1.0/v2.1.0).
#
# --dry-run: all the checks, then prints what it would build, push or copy.
#
# Credentials: podman and skopeo read the registry login that
# `podman login ghcr.io` stored (REGISTRY_AUTH_FILE or the default
# auth.json); nothing is ever passed on a command line.
#
# Environment (tests): PODMAN, SKOPEO, PUBLISH_STAGE (the 01/ directory).
# Exit: 0 done; 1 refused or failed; 64 usage.
set -euo pipefail
stage=${PUBLISH_STAGE:-$(cd "$(dirname "$0")/.." && pwd)}
PODMAN=${PODMAN:-podman}
SKOPEO=${SKOPEO:-skopeo}
PLATFORM=linux/amd64

# tag  manifest digest  (01/docs/design-v2/06-reference.md section 6.2)
HISTORY=(
	"v1.0.0 sha256:e34801f1457f6026984c3b9095cd32eecb91f158d32b8475e5374a554ccd9fb3"
	"v2.0.0 sha256:935764966ab0f6d68e1b292bf18befcdf33310ceed073d17fa77a43ded69b891"
	"v1.1.0 sha256:e767bd6d12d316733c826da07b387674791c4f7e1e11cff8d5f1b8aa0459bdea"
	"v2.1.0 sha256:082a7412ec4de56f160c7d1bbe91162a7f119a4e0742421345d866c609648cf4"
)

usage() {
	sed -n '5,6p' "$0" | sed 's/^# \{0,1\}/usage: /' >&2
	exit 64
}
say() { printf 'publish-images: %s\n' "$*"; }
die() {
	printf 'publish-images: %s\n' "$*" >&2
	exit 1
}

mode=publish dry_run=0
while [ $# -gt 0 ]; do
	case $1 in
	--retag-history) mode=retag; shift ;;
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

# remote_digest REF: prints the manifest digest of REF (docker://...), or
# "absent" when the registry says it does not exist. Any other error (no
# network, no permission, ...) fails: absence must be proven, not assumed.
remote_digest() {
	local ref=$1 out
	if out=$("$SKOPEO" inspect --format '{{.Digest}}' "docker://$ref" 2>"$tmp/inspect.err"); then
		[[ $out =~ ^sha256:[0-9a-f]{64}$ ]] || {
			echo "unexpected digest for $ref: $out" >&2
			return 1
		}
		echo "$out"
	elif grep -qiE 'manifest unknown|manifest_unknown|not found|name unknown' "$tmp/inspect.err"; then
		echo absent
	else
		echo "cannot tell whether $ref exists: $(head -c 300 "$tmp/inspect.err" | tr '\n' ' ')" >&2
		return 1
	fi
}

# require_absent TAG...: refuses when any REPO:TAG exists (all are checked).
require_absent() {
	local t d bad=()
	for t in "$@"; do
		d=$(remote_digest "$repo:$t") || die "refusing: $repo:$t could not be checked"
		if [ "$d" != absent ]; then
			bad+=("$repo:$t ($d)")
		fi
	done
	[ ${#bad[@]} -eq 0 ] ||
		die "refusing: already published, tags are never overwritten: ${bad[*]}; bump KOUPA_V1_TAG/KOUPA_V2_TAG in images.env"
}

require_login() {
	local host=${repo%%/*}
	"$PODMAN" login --get-login "$host" >/dev/null 2>&1 ||
		die "not logged in to $host: run podman login $host first (the token is read from there, never from argv)"
}

# ------------------------------------------------------------ retag-history
if [ "$mode" = retag ]; then
	todo=()
	for row in "${HISTORY[@]}"; do
		read -r tag digest <<<"$row"
		src=$(remote_digest "$repo@$digest") || die "refusing: $repo@$digest could not be checked"
		[ "$src" = "$digest" ] || die "refusing: $repo@$digest is not on the registry ($src)"
		have=$(remote_digest "$repo:$tag") || die "refusing: $repo:$tag could not be checked"
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
		# Checked again right before the copy: never overwrite.
		have=$(remote_digest "$repo:$tag") || die "refusing: $repo:$tag could not be checked"
		[ "$have" = absent ] || die "refusing: $repo:$tag appeared meanwhile ($have)"
		"$SKOPEO" copy --preserve-digests "docker://$repo@$digest" "docker://$repo:$tag" >"$tmp/copy.log" 2>&1 ||
			die "skopeo copy to $repo:$tag failed: $(tail -c 300 "$tmp/copy.log" | tr '\n' ' ')"
		got=$(remote_digest "$repo:$tag") || die "cannot read back $repo:$tag"
		[ "$got" = "$digest" ] || die "$repo:$tag is $got after the copy, want $digest"
		say "tagged $repo:$tag $got"
	done
	exit 0
fi

# ------------------------------------------------------------------ publish
semver='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
[[ ${KOUPA_V1_TAG:-} =~ ^v1\.$semver$ ]] || die "refusing: KOUPA_V1_TAG=${KOUPA_V1_TAG:-} is not v1.MINOR.PATCH"
[[ ${KOUPA_V2_TAG:-} =~ ^v2\.$semver$ ]] || die "refusing: KOUPA_V2_TAG=${KOUPA_V2_TAG:-} is not v2.MINOR.PATCH"
[ "${KOUPA_V1_TAG#v1.}" = "${KOUPA_V2_TAG#v2.}" ] ||
	die "refusing: KOUPA_V1_TAG=$KOUPA_V1_TAG and KOUPA_V2_TAG=$KOUPA_V2_TAG differ in MINOR.PATCH (both lines move together)"

git -C "$stage" rev-parse --verify -q HEAD >/dev/null || die "$stage is not in a git repository with a commit"
dirty=$(git -C "$stage" status --porcelain --untracked-files=normal -- . 2>&1) || die "git status failed: $dirty"
[ -z "$dirty" ] || die "refusing: the working tree under $stage is not clean; commit first:
$dirty"
commit=$(git -C "$stage" rev-parse HEAD)
prefix=$(git -C "$stage" rev-parse --show-prefix)

require_absent "$KOUPA_V1_TAG" "$KOUPA_V2_TAG"

if [ "$dry_run" = 1 ]; then
	say "dry run: would build $repo:$KOUPA_V1_TAG and $repo:$KOUPA_V2_TAG from $commit and push them"
	exit 0
fi
require_login

ctx=$tmp/src
mkdir -p "$ctx"
git -C "$stage" archive --format=tar "HEAD:${prefix}" | tar -x -C "$ctx" || die "git archive of HEAD failed"

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
	img=$repo:$tag
	# Checked again right before the push: never overwrite.
	require_absent "$tag"
	"$PODMAN" push --digestfile "$tmp/digest" "$img" || die "push of $img failed"
	pushed=$(cat "$tmp/digest")
	got=$(remote_digest "$img") || die "cannot read back $img"
	[ "$got" = "$pushed" ] || die "$img is $got on the registry, push reported $pushed"
	say "pushed $img $got"
done
say "done: record the digests in 01/docs/design-v2/06-reference.md section 6.2"
