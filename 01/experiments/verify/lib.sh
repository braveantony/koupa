# Shared helpers of the measurement-tool checks (plan A14, design 5.6).
# Sourced by mutation.sh, protocol.sh and compare-tools.sh; builds on the
# critical-timing helpers (deploy, pull secret, restore to step 4 at exit).
#
# Environment:
#   VERIFY_EVIDENCE  evidence root (default evidence/verify-<ts>)
#   VERIFY_SUMMARY   file every PASS/FAIL line is appended to (default
#                    $VERIFY_EVIDENCE/summary.txt)
#   VERIFY_SKIP_DEPLOY 1 = use the cluster as it is (to show the checks
#                    failing while the setting under test is not applied)
#   CRIT_NO_RESTORE  1 = do not restore step 4 at exit

verify_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
VERIFY_EVIDENCE=${VERIFY_EVIDENCE:-$(cd "$verify_dir/../.." && pwd)/evidence/verify-$(date -u +%Y%m%dT%H%M%SZ)}
mkdir -p "$VERIFY_EVIDENCE"
VERIFY_EVIDENCE=$(cd "$VERIFY_EVIDENCE" && pwd)
export VERIFY_EVIDENCE
VERIFY_SUMMARY=${VERIFY_SUMMARY:-$VERIFY_EVIDENCE/summary.txt}
CRIT_EVIDENCE=$VERIFY_EVIDENCE
CRIT_SUMMARY=$VERIFY_SUMMARY
# shellcheck source=../critical/lib.sh
. "$verify_dir/../critical/lib.sh"

build_rush_hour() {
	mkdir -p "$stage/bin"
	(cd "$stage" && CGO_ENABLED=0 timeout 300 scripts/go build -o "$RH_BIN" ./rush-hour) >&2
}

# verify_deploy NAME DIR: apply experiments/verify/overlays/NAME with the
# experiment image (or nothing with VERIFY_SKIP_DEPLOY=1).
verify_deploy() {
	local name=$1 dir=$2
	if [ "${VERIFY_SKIP_DEPLOY:-0}" = 1 ]; then
		log "VERIFY_SKIP_DEPLOY=1: using the cluster as it is (not $name)"
		echo "skipped: cluster as it is" >"$dir/deploy.txt"
		return 0
	fi
	log "applying overlay verify/$name ($CRIT_APP_IMAGE)"
	rm -f "$CRIT_EVIDENCE/.deployed"
	pull_secret_up || return 1
	apply_overlay "$verify_dir/overlays/$name" "$CRIT_APP_IMAGE" "$dir/manifest.yaml" || { log "apply $name failed"; return 1; }
	echo "verify/$name $CRIT_APP_IMAGE" >"$dir/deploy.txt"
}

# vcheck DIR: python expectations on stdin, with helpers:
#   S                  DIR/scorecard.json ({} if none)
#   app_lines()        every DIR/app/<uid>.log line as a dict, with "_uid"
#   boards()           DIR/boards.jsonl as dicts
#   audit(name)        the scorecard's DB audit: cursor, orders, ready (None if missing)
#   ok(cond, text)     one expectation
# Writes DIR/checks.txt; exit 0 only if every expectation holds.
vcheck() {
	local dir=$1
	{
		cat <<'PY'
import glob, json, os, sys, collections
D = sys.argv[1]
def load(p, default=None):
    try:
        return json.load(open(os.path.join(D, p)))
    except Exception:
        return default
S = load("scorecard.json", {})
def jsonl(p):
    out = []
    try:
        for l in open(os.path.join(D, p), errors="replace"):
            l = l.strip()
            if l:
                try:
                    out.append(json.loads(l))
                except ValueError:
                    pass
    except OSError:
        pass
    return out
def app_lines():
    out = []
    for f in sorted(glob.glob(os.path.join(D, "app", "*.log"))):
        uid = os.path.basename(f)[:-4]
        for l in open(f, errors="replace"):
            i = l.find("{")
            if i < 0:
                continue
            try:
                m = json.loads(l[i:])
            except ValueError:
                continue
            m["_uid"] = uid
            out.append(m)
    return out
def boards():
    return jsonl("boards.jsonl")
def audit(name):
    """The DB audit the scorecard used (facts.audit: cursor, orders, ready)."""
    return ((S.get("facts") or {}).get("audit") or {}).get(name)
FAILED = []
def ok(cond, text):
    print(("ok   " if cond else "FAIL ") + text)
    if not cond:
        FAILED.append(text)
_of = os.path.join(D, "orchestration.failed")
ok(not os.path.exists(_of), "every orchestration step completed" +
   (": " + open(_of).read().strip().replace("\n", "; ") if os.path.exists(_of) else ""))
PY
		echo 'try:'
		sed 's/^/    /'
		echo 'except Exception as e:'
		echo '    import traceback; traceback.print_exc()'
		echo '    ok(False, "expectations could not be evaluated: %r" % e)'
		echo 'sys.exit(1 if FAILED else 0)'
	} >"$dir/checks.py"
	python3 -I "$dir/checks.py" "$dir" >"$dir/checks.txt" 2>&1
	local rc=$?
	sed 's/^/    /' "$dir/checks.txt" >&2
	return $rc
}
