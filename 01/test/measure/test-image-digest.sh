#!/usr/bin/env bash
# Every running koupa Pod must run the digest its registry tag resolves to
# now (a node with an older image cached under the same tag is caught);
# measure.sh resolves the koupa tags and the rush-hour image ID once per
# run, records them in image-digests.json, runs rush-hour by that ID.
. "$(dirname "$0")/lib.sh"
setup_fakes
V1=$(printf 'sha256:%064d' 1)

make_live step-0
out=$("$STAGE/scripts/preflight.sh" step-0 2>&1)
rc=$?
check "matching digest passes (rc=$rc)" [ "$rc" -eq 0 ]
check "image-digest ok line" grep -q "^ok   image-digest:.*$V1" <<<"$out"
check "preflight resolved the tag with skopeo" grep -q 'skopeo inspect .*docker://ghcr.io/braveantony/koupa:v1' "$FAKE/calls.log"

out=$("$STAGE/scripts/preflight.sh" step-0 --digest "$V1" 2>&1)
check "--digest given: passes" [ $? -eq 0 ]

# One Pod still runs an older image under the same tag.
python3 -I - "$FAKE/pods.json" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p["items"][2]["status"]["containerStatuses"][0]["imageID"] = "ghcr.io/braveantony/koupa@sha256:" + "9" * 64
json.dump(p, open(sys.argv[1], "w"))
PY
out=$("$STAGE/scripts/preflight.sh" step-0 2>&1)
rc=$?
check "stale Pod digest fails (rc=$rc)" [ "$rc" -eq 1 ]
check "names the Pod" grep -q '^FAIL image-digest:.*koupa-abc-2' <<<"$out"

# The node holds the same image under a second name (the experiments image
# built from the same commit has the same manifest digest): CRI-O may report
# that name in status.image and imageID. The digest is the one the v1 tag
# resolves to, so the Pod runs the wanted image.
make_live step-0
python3 -I - "$FAKE/pods.json" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
for it in p["items"]:
    st = it["status"]["containerStatuses"][0]
    st["image"] = "ghcr.io/braveantony/koupa-experiments:app-0000000"
    st["imageID"] = "ghcr.io/braveantony/koupa-experiments@sha256:" + "0" * 63 + "1"
json.dump(p, open(sys.argv[1], "w"))
PY
out=$("$STAGE/scripts/preflight.sh" step-0 2>&1)
rc=$?
check "same image under another name passes (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || grep FAIL <<<"$out" | sed 's/^/    /'

# Another name and another digest: really another image.
python3 -I - "$FAKE/pods.json" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
st = p["items"][1]["status"]["containerStatuses"][0]
st["imageID"] = "ghcr.io/braveantony/koupa-experiments@sha256:" + "8" * 64
json.dump(p, open(sys.argv[1], "w"))
PY
out=$("$STAGE/scripts/preflight.sh" step-0 2>&1)
rc=$?
check "another name and digest fails (rc=$rc)" [ "$rc" -eq 1 ]
check "spec names the Pod" grep -q '^FAIL spec:.*koupa-abc-1 runs image' <<<"$out"

make_live step-0
touch "$FAKE/skopeo-fail"
out=$("$STAGE/scripts/preflight.sh" step-0 2>&1)
check "unresolvable tag fails" grep -q '^FAIL image-digest: cannot resolve' <<<"$out"
rm -f "$FAKE/skopeo-fail"

# measure.sh: resolved once per run, recorded, rush-hour run by image ID.
unset RUSH_HOUR_BIN
echo 25 >"$FAKE/rh-backdate"
make_live step-0
: >"$FAKE/calls.log"
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
check "control: exit 0 (rc=$rc)" [ "$rc" -eq 0 ]
[ "$rc" -eq 0 ] || tail -15 "$T/out" | sed 's/^/    /'
run=$(latest_run)
check "image-digests.json records v1, v2 and rush-hour image ID" python3 -I -c '
import json, sys
d = json.load(open(sys.argv[1]))
ok = (d["koupa"]["ghcr.io/braveantony/koupa:v1"]["digest"] == "sha256:" + "0" * 63 + "1"
      and d["koupa"]["ghcr.io/braveantony/koupa:v2"]["digest"] == "sha256:" + "0" * 63 + "2"
      and d["rush_hour"]["image_id"] == "fa4e1d" + "0" * 57 + "1")
sys.exit(0 if ok else 1)' "$run/image-digests.json"
check "koupa tags resolved once per run" [ "$(grep -c '^skopeo inspect' "$FAKE/calls.log")" -eq 2 ]
check "preflight got the resolved digest" grep -q "^ok   image-digest:.*$V1" "$run/preflight.txt"
check "rush-hour run by image ID" grep -q 'podman run .* fa4e1d0*1 run ' "$FAKE/calls.log"
check "evidence index has image-digests.json" grep -q '^OK      image-digests.json' "$run/evidence-index.txt"
check "images.json has the rush-hour image ID" grep -q 'fa4e1d0*1' "$run/images.json"

# Registry moved on: a Pod runs another digest -> no measurement.
echo "sha256:$(printf '%064d' 7)" >"$FAKE/digest-v1"
touch "$FAKE/apply-stale" # the nodes keep running the old image under the tag
"$STAGE/measure.sh" control --runs 1 --duration 30 >"$T/out" 2>&1
rc=$?
check "digest mismatch: exit 1 (rc=$rc)" [ "$rc" -eq 1 ]
check "says image-digest" grep -q '量測前核對不符：image-digest' "$T/out"
rm -f "$FAKE/digest-v1" "$FAKE/apply-stale"
export RUSH_HOUR_BIN=$FAKE/bin/rush-hour
finish
