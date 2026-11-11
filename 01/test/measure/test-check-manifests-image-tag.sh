#!/usr/bin/env bash
# scripts/check-manifests.sh: every overlay renders the image
# KOUPA_REPO:KOUPA_V1_TAG of images.env; a base that lags behind a version
# bump, or still names the legacy floating tag, fails. Works on a copy.
. "$(dirname "$0")/lib.sh"
T=$(mktemp -d "${TMPDIR:-/tmp}/manifest-test.XXXXXX")
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/stage"
cp -r "$STAGE/scripts" "$STAGE/k8s" "$STAGE/images.env" "$T/stage/"
CM=$T/stage/scripts/check-manifests.sh
. "$STAGE/images.env"

for o in base step-3 step-5 calibrate; do
	out=$("$CM" "$o" 2>&1)
	check "$o passes with images.env as is (rc=$?)" grep -q "^PASS $o" <<<"$out"
	check "$o: tag line names $KOUPA_V1_TAG" grep -q "^ok   $o: image tag is $KOUPA_V1_TAG" <<<"$out"
done

# images.env bumped, k8s/base not: every overlay fails.
sed -i 's/^KOUPA_V1_TAG=.*/KOUPA_V1_TAG=v1.99.0/' "$T/stage/images.env"
for o in base step-5; do
	out=$("$CM" "$o" 2>&1)
	rc=$?
	check "$o fails after a bump without k8s/base (rc=$rc)" [ "$rc" -eq 1 ]
	check "$o names both tags" grep -q "^FAIL $o: image tag is v1.99.0 .*(got $KOUPA_V1_TAG)" <<<"$out"
done

# k8s/base back on the legacy floating tag.
cp "$STAGE/images.env" "$T/stage/images.env"
sed -i -E 's#(image: ghcr.io/braveantony/koupa:)v[0-9.]+#\1v1#' "$T/stage/k8s/base/deployment.yaml"
out=$("$CM" base 2>&1)
rc=$?
check "legacy :v1 in k8s/base fails (rc=$rc)" [ "$rc" -eq 1 ]
check "says got v1" grep -q "^FAIL base: image tag is $KOUPA_V1_TAG .*(got v1)" <<<"$out"

# Another repository fails too.
sed -i -E 's#image: ghcr.io/braveantony/koupa:v1#image: ghcr.io/someone/koupa:'"$KOUPA_V1_TAG"'#' "$T/stage/k8s/base/deployment.yaml"
out=$("$CM" base 2>&1)
check "other repository fails (rc=$?)" grep -q "^FAIL base: image is $KOUPA_REPO " <<<"$out"

finish
