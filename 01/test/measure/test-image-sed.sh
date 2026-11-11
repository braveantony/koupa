#!/usr/bin/env bash
# scripts/lib/image-sed.sh set_koupa_image: replaces only a YAML image:
# whose whole value is KOUPA_REPO:vN[.N[.N]] (the repository matched
# literally); neighbours and longer tags stay as they are.
. "$(dirname "$0")/lib.sh"
T=$(mktemp -d "${TMPDIR:-/tmp}/image-sed.XXXXXX")
trap 'rm -rf "$T"' EXIT
NEW=$KOUPA_REPO:v2.9.1

cat >"$T/in.yaml" <<EOF
        image: $KOUPA_REPO:$KOUPA_V1_TAG
        - image: $KOUPA_REPO:v1
          image: "$KOUPA_REPO:v2.2.0"
        image: ghcr.io/braveantony/koupa-experiments:app-5acbfde
        image: ghcr.io/braveantony/rush-hour:0.1.0
        image: $KOUPA_REPO:v1.2.0-rc1
        image: $KOUPA_REPO:v1.2.0.1
        image: $KOUPA_REPO:v1.2.0x
        image: ghcrXio/braveantony/koupa:v1.2.0
        image: example.com/$KOUPA_REPO:v1.2.0
          value: $KOUPA_REPO:v1.2.0
        # image: $KOUPA_REPO:v1.2.0 in a comment
EOF
out=$(set_koupa_image "$NEW" <"$T/in.yaml")
rc=$?
check "rc 0 ($rc)" [ "$rc" -eq 0 ]
line() { sed -n "${1}p" <<<"$out"; }
check "current tag replaced" [ "$(line 1)" = "        image: $NEW" ]
check "legacy floating tag in a list item replaced" [ "$(line 2)" = "        - image: $NEW" ]
check "quoted value replaced" [ "$(line 3)" = "          image: $NEW" ]
for n in 4 5 6 7 8 9 10 11 12; do
	check "line $n untouched: $(sed -n "${n}p" "$T/in.yaml" | sed 's/^ *//')" [ "$(line "$n")" = "$(sed -n "${n}p" "$T/in.yaml")" ]
done

printf '        image: ghcr.io/braveantony/koupa-experiments:app-1\n' >"$T/none.yaml"
set_koupa_image "$NEW" <"$T/none.yaml" >/dev/null 2>"$T/err"
check "nothing to replace fails (rc=$?)" grep -q "no image:" "$T/err"
set_koupa_image 'x#y&z' <"$T/in.yaml" >/dev/null 2>&1
check "an image with sed metacharacters is refused (rc=$?)" [ $? -ne 0 ]

# The real overlays: one koupa image each, swapped.
for o in k8s/base k8s/steps/step-5 k8s/calibrate; do
	got=$("$REAL_KUBECTL" kustomize "$STAGE/$o" | set_koupa_image "$NEW" | grep -c "image: $NEW\$")
	check "$o: the image swapped ($got)" [ "$got" -eq 1 ]
done
finish
