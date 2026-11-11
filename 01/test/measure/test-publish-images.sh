#!/usr/bin/env bash
# scripts/publish-images.sh never overwrites a published tag: it checks the
# registry first (skopeo inspect), refuses on an existing tag or on any
# error that does not prove absence, builds only from a clean tree, and
# --retag-history copies the recorded digests without overwriting. Fake
# podman and skopeo (a registry in $FAKE/reg); a scratch git repo as 01/.
. "$(dirname "$0")/lib.sh"
T=$(mktemp -d "${TMPDIR:-/tmp}/publish-test.XXXXXX")
trap 'rm -rf "$T"' EXIT
FAKE=$T/fake
mkdir -p "$FAKE/bin" "$FAKE/reg"
export FAKE PATH=$FAKE/bin:$PATH
export PODMAN=$FAKE/bin/podman SKOPEO=$FAKE/bin/skopeo
SCRIPT=$STAGE/scripts/publish-images.sh
REPO=ghcr.io/braveantony/koupa
D() { printf 'sha256:%064d' "$1"; }
H10=sha256:e34801f1457f6026984c3b9095cd32eecb91f158d32b8475e5374a554ccd9fb3
H20=sha256:935764966ab0f6d68e1b292bf18befcdf33310ceed073d17fa77a43ded69b891
H11=sha256:e767bd6d12d316733c826da07b387674791c4f7e1e11cff8d5f1b8aa0459bdea
H21=sha256:082a7412ec4de56f160c7d1bbe91162a7f119a4e0742421345d866c609648cf4

# Registry: $FAKE/reg/tag-<tag> holds the tag's digest, $FAKE/reg/blob-<digest>
# marks a manifest that exists; $FAKE/reg-error makes inspect fail otherwise.
cat >"$FAKE/bin/skopeo" <<'EOF'
#!/usr/bin/env bash
echo "skopeo $*" >>"$FAKE/calls.log"
case $1 in
inspect)
	ref=${!#}; ref=${ref#docker://}
	if [ -f "$FAKE/reg-error" ]; then cat "$FAKE/reg-error" >&2; exit 1; fi
	case $ref in
	*@sha256:*) d=${ref#*@}; [ -f "$FAKE/reg/blob-$d" ] && { echo "$d"; exit 0; } ;;
	*) t=${ref##*:}; [ -f "$FAKE/reg/tag-$t" ] && { cat "$FAKE/reg/tag-$t"; exit 0; } ;;
	esac
	echo "time=x level=fatal msg=\"reading manifest in $ref: manifest unknown\"" >&2
	exit 2 ;;
copy)
	src=${@: -2:1}; dst=${!#}
	d=${src#*@}; t=${dst##*:}
	[ -f "$FAKE/reg/blob-$d" ] || { echo "no such manifest $d" >&2; exit 1; }
	echo "$d" >"$FAKE/reg/tag-$t"
	exit 0 ;;
esac
exit 1
EOF
# podman: build records the VERSION build-arg per tag; run --version prints
# it; push stores digest D(n) on the registry; login --get-login answers
# unless $FAKE/no-login.
cat >"$FAKE/bin/podman" <<'EOF'
#!/usr/bin/env bash
echo "podman $*" >>"$FAKE/calls.log"
case $1 in
login) [ -f "$FAKE/no-login" ] && { echo "Error: not logged into ghcr.io" >&2; exit 125; }; echo tester ;;
build)
	ver="" tag=""
	while [ $# -gt 0 ]; do
		case $1 in
		--build-arg) ver=${2#VERSION=}; shift ;;
		-t) tag=${2##*:}; shift ;;
		esac
		shift
	done
	echo "$ver" >"$FAKE/built-$tag" ;;
run) img=${@: -2:1}; cat "$FAKE/built-${img##*:}" ;;
push)
	df=$3 img=$4 t=${4##*:}
	n=$(($(ls "$FAKE/reg" | wc -l) + 100))
	d=$(printf 'sha256:%064d' "$n")
	touch "$FAKE/reg/blob-$d"; echo "$d" >"$FAKE/reg/tag-$t"; printf '%s' "$d" >"$df" ;;
esac
exit 0
EOF
chmod +x "$FAKE/bin/skopeo" "$FAKE/bin/podman"

# A scratch repository whose 01/ has the real images.env.
mkdir -p "$T/repo/01/app"
cp "$STAGE/images.env" "$T/repo/01/images.env"
echo 'FROM scratch' >"$T/repo/01/app/Containerfile"
git -C "$T/repo" init -q
git -C "$T/repo" -c user.name=t -c user.email=t@t add -A
git -C "$T/repo" -c user.name=t -c user.email=t@t commit -qm init
export PUBLISH_STAGE=$T/repo/01
. "$STAGE/images.env"

reset_reg() {
	rm -rf "$FAKE/reg" "$FAKE/reg-error" "$FAKE/no-login" "$FAKE"/built-*
	mkdir -p "$FAKE/reg"
	for d in $H10 $H20 $H11 $H21; do touch "$FAKE/reg/blob-$d"; done
	: >"$FAKE/calls.log"
}
run() {
	out=$("$SCRIPT" "$@" 2>&1)
	rc=$?
}
no_change() { ! grep -qE '^podman (build|push)|^skopeo copy' "$FAKE/calls.log"; }

echo "-- publish: a tag that already exists is never overwritten"
reset_reg
D 7 >"$FAKE/reg/tag-$KOUPA_V2_TAG"
run
check "refused (rc=$rc)" [ "$rc" -eq 1 ]
check "names the existing tag" grep -q "refusing: already published.*$REPO:$KOUPA_V2_TAG" <<<"$out"
check "nothing built or pushed" no_change
check "the registry tag still has its digest" [ "$(cat "$FAKE/reg/tag-$KOUPA_V2_TAG")" = "$(D 7)" ]
check "both tags were checked first" bash -c 'grep -q "skopeo inspect .*koupa:$1" "$3" && grep -q "skopeo inspect .*koupa:$2" "$3"' _ "$KOUPA_V1_TAG" "$KOUPA_V2_TAG" "$FAKE/calls.log"

echo "-- publish: a registry error is not taken as absent"
reset_reg
echo 'Error: unauthorized: authentication required' >"$FAKE/reg-error"
run
check "refused (rc=$rc)" [ "$rc" -eq 1 ]
check "says it could not check" grep -q "cannot tell whether.*unauthorized" <<<"$out"
check "nothing built or pushed" no_change

echo "-- publish: dirty tree"
reset_reg
echo x >"$T/repo/01/stray.txt"
run
check "refused (rc=$rc)" [ "$rc" -eq 1 ]
check "says not clean" grep -q "not clean" <<<"$out"
check "nothing built or pushed" no_change
rm -f "$T/repo/01/stray.txt"

echo "-- publish: tags must be semver of their line"
for pair in "v1 v2.2.0" "v1.2 v2.2.0" "v2.2.0 v2.2.0" "v1.2.0 v2.3.0" "v1.02.0 v2.02.0"; do
	reset_reg
	read -r a b <<<"$pair"
	sed -i "s/^KOUPA_V1_TAG=.*/KOUPA_V1_TAG=$a/; s/^KOUPA_V2_TAG=.*/KOUPA_V2_TAG=$b/" "$T/repo/01/images.env"
	run
	check "$a / $b refused (rc=$rc)" [ "$rc" -eq 1 ]
	check "$a / $b: nothing built" no_change
done
git -C "$T/repo" checkout -q -- 01/images.env

echo "-- publish: not logged in"
reset_reg
touch "$FAKE/no-login"
run
check "refused (rc=$rc)" [ "$rc" -eq 1 ]
check "says podman login" grep -q "podman login" <<<"$out"
check "nothing built or pushed" no_change

echo "-- publish: dry run"
reset_reg
run --dry-run
check "rc=0 ($rc)" [ "$rc" -eq 0 ]
check "nothing built or pushed" no_change

echo "-- publish: absent tags are built from HEAD and pushed"
reset_reg
run
check "rc=0 ($rc): $out" [ "$rc" -eq 0 ]
head=$(git -C "$T/repo" rev-parse HEAD)
check "v1 built with VERSION=$KOUPA_V1_TAG" grep -q "^podman build .*--build-arg VERSION=$KOUPA_V1_TAG .*-t $REPO:$KOUPA_V1_TAG " "$FAKE/calls.log"
check "v2 built with VERSION=$KOUPA_V2_TAG" grep -q "^podman build .*--build-arg VERSION=$KOUPA_V2_TAG .*-t $REPO:$KOUPA_V2_TAG " "$FAKE/calls.log"
check "labelled with the commit" grep -q "org.opencontainers.image.revision=$head" "$FAKE/calls.log"
check "built from an export, not the working tree" bash -c '! grep "^podman build" "$1" | grep -q " $2\$"' _ "$FAKE/calls.log" "$T/repo/01"
check "both pushed" bash -c '[ "$(grep -c "^podman push" "$1")" -eq 2 ]' _ "$FAKE/calls.log"
check "prints both digests" bash -c 'grep -q "pushed .*:$1 sha256:" <<<"$3" && grep -q "pushed .*:$2 sha256:" <<<"$3"' _ "$KOUPA_V1_TAG" "$KOUPA_V2_TAG" "$out"
check "no credential on any command line" bash -c '! grep -qiE -- "--creds|--password|-p |token" "$1"' _ "$FAKE/calls.log"
run
check "a second publish of the same tags is refused (rc=$rc)" [ "$rc" -eq 1 ]

echo "-- retag-history: a tag on another digest stops everything"
reset_reg
D 5 >"$FAKE/reg/tag-v2.1.0"
run --retag-history
check "refused (rc=$rc)" [ "$rc" -eq 1 ]
check "names v2.1.0" grep -q "koupa:v2.1.0 exists with $(D 5)" <<<"$out"
check "nothing copied" no_change
check "v2.1.0 untouched" [ "$(cat "$FAKE/reg/tag-v2.1.0")" = "$(D 5)" ]

echo "-- retag-history: a missing source digest stops everything"
reset_reg
rm "$FAKE/reg/blob-$H11"
run --retag-history
check "refused (rc=$rc)" [ "$rc" -eq 1 ]
check "nothing copied" no_change

echo "-- retag-history: dry run"
reset_reg
run --retag-history --dry-run
check "rc=0 ($rc)" [ "$rc" -eq 0 ]
check "nothing copied" no_change
check "lists 4 copies" [ "$(grep -c 'would copy' <<<"$out")" -eq 4 ]

echo "-- retag-history: copies the recorded digests, skips a tag already right"
reset_reg
echo "$H10" >"$FAKE/reg/tag-v1.0.0"
run --retag-history
check "rc=0 ($rc): $out" [ "$rc" -eq 0 ]
check "v1.0.0 skipped" grep -q "koupa:v1.0.0 is already $H10: skipped" <<<"$out"
check "3 copies" [ "$(grep -c '^skopeo copy' "$FAKE/calls.log")" -eq 3 ]
check "copies keep the digest" bash -c '! grep "^skopeo copy" "$1" | grep -qv -- "--preserve-digests"' _ "$FAKE/calls.log"
for row in "v2.0.0 $H20" "v1.1.0 $H11" "v2.1.0 $H21"; do
	read -r t d <<<"$row"
	check "$t -> $d" grep -q "^skopeo copy --preserve-digests docker://$REPO@$d docker://$REPO:$t\$" "$FAKE/calls.log"
	check "$t on the registry" [ "$(cat "$FAKE/reg/tag-$t")" = "$d" ]
done
check "legacy v1/v2 not touched" bash -c '! grep -qE "koupa:v[12]( |$)" "$1"' _ "$FAKE/calls.log"
run --retag-history
check "rerun: nothing to do (rc=$rc)" bash -c '[ "$1" -eq 0 ] && grep -q "nothing to do" <<<"$2"' _ "$rc" "$out"

finish
