#!/usr/bin/env bash
# Sourcing experiments/critical/lib.sh (or experiments/verify/lib.sh) and
# exiting must not touch any cluster: no trap is installed by sourcing, and
# the exit cleanup restores step 5 only after crit_arm. A fake kubectl
# (and podman/skopeo) records every call; KUBECONFIG points nowhere.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
crit=$(cd "$here/.." && pwd)
verify=$(cd "$crit/../verify" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
for t in kubectl podman skopeo; do
	cat >"$tmp/bin/$t" <<EOT
#!/usr/bin/env bash
echo "$t \$*" >>"$tmp/calls.log"
[ "$t" = kubectl ] && [ "\${1:-}" = kustomize ] && { printf 'kind: Deployment\n        image: ghcr.io/braveantony/koupa:v1\n'; exit 0; }
exit 1
EOT
	chmod +x "$tmp/bin/$t"
done
# kubectl: kustomize renders one koupa image; `apply -f -` of a Secret
# succeeds and, with $tmp/kill-on-secret, TERMs the shell in $tmp/parent.pid
# right after it (an interrupt between the secret and anything else); every
# other call fails.
cat >"$tmp/bin/kubectl" <<EOT
#!/usr/bin/env bash
echo "kubectl \$*" >>"$tmp/calls.log"
case "\$*" in
kustomize*) printf 'kind: Deployment\n        image: ghcr.io/braveantony/koupa:v1\n'; exit 0 ;;
"apply -f -")
	in=\$(cat)
	if grep -q '"kind": "Secret"' <<<"\$in"; then
		echo "kubectl applied-secret" >>"$tmp/calls.log"
		[ -f "$tmp/kill-on-secret" ] && kill -TERM "\$(cat "$tmp/parent.pid")"
		exit 0
	fi ;;
"-n koupa delete secret"*) exit 0 ;;
esac
exit 1
EOT
chmod +x "$tmp/bin/kubectl"
mkdir -p "$tmp/run/containers"
echo '{"auths":{"ghcr.io":{"auth":"ZmFrZTpmYWtl"}}}' >"$tmp/run/containers/auth.json"
export PATH=$tmp/bin:$PATH KUBECONFIG=/dev/null XDG_RUNTIME_DIR=$tmp/run
export CRIT_EVIDENCE=$tmp/ev VERIFY_EVIDENCE=$tmp/vev
fails=0
calls() { [ -f "$tmp/calls.log" ] && grep -c "^$1 " "$tmp/calls.log" || echo 0; }
case_() { # NAME WANT_KUBECTL(0|some) SCRIPT
	local name=$1 want=$2 script=$3 n
	rm -f "$tmp/calls.log"
	timeout 60 bash -c "$script" >"$tmp/out" 2>&1
	n=$(calls kubectl)
	if { [ "$want" = 0 ] && [ "$n" = 0 ]; } || { [ "$want" = some ] && [ "$n" -gt 0 ]; }; then
		echo "ok   $name (kubectl calls: $n)"
	else
		echo "FAIL $name (kubectl calls: $n, want $want)"; sed 's/^/    /' "$tmp/calls.log" 2>/dev/null
		fails=$((fails + 1))
	fi
}
case_ "source critical/lib.sh, exit" 0 ". '$crit/lib.sh'; exit 0"
case_ "source critical/lib.sh, exit 1" 0 ". '$crit/lib.sh'; exit 1"
case_ "source critical/lib.sh, crit_init, exit (not armed)" 0 ". '$crit/lib.sh'; crit_init; exit 0"
case_ "source critical/lib.sh, crit_init, TERM (not armed)" 0 ". '$crit/lib.sh'; crit_init; kill -TERM \$\$; sleep 5"
case_ "source verify/lib.sh, exit" 0 ". '$verify/lib.sh'; exit 0"
case_ "source verify/lib.sh, crit_init, exit (not armed)" 0 ". '$verify/lib.sh'; crit_init; exit 0"
case_ "armed with CRIT_NO_RESTORE=1: no restore" 0 "CRIT_NO_RESTORE=1; . '$crit/lib.sh'; crit_init; crit_arm; exit 0"
# Control: once armed, the cleanup does try to restore (the fake apply fails).
case_ "armed: the exit cleanup restores step 5" some ". '$crit/lib.sh'; crit_init; crit_arm; exit 0"
rm -f "$tmp/calls.log"
timeout 60 bash -c ". '$crit/lib.sh'; crit_init; crit_arm; exit 0" >/dev/null 2>&1
if grep -q '^kubectl apply' "$tmp/calls.log" 2>/dev/null; then echo "ok   armed restore calls kubectl apply"; else echo "FAIL armed restore calls kubectl apply"; fails=$((fails + 1)); fi
# Interrupted right after the pull secret was applied: the cleanup must
# restore (pull_secret_up armed it in this shell, not in its pipeline).
restore_after() { # NAME SCRIPT: the restore's Deployment apply comes after the write
	local name=$1 script=$2
	rm -f "$tmp/calls.log"
	timeout 60 bash -c "echo \$\$ >'$tmp/parent.pid'; $script" >"$tmp/out" 2>&1
	if awk '/^kubectl (applied-secret|-n koupa delete secret)/{w=NR} /^kubectl apply -f \//{if (w) r=NR} END{exit !(w && r > w)}' "$tmp/calls.log"; then
		echo "ok   $name"
	else
		echo "FAIL $name"; sed 's/^/    /' "$tmp/calls.log" "$tmp/out"
		fails=$((fails + 1))
	fi
}
touch "$tmp/kill-on-secret"
restore_after "TERM right after the pull secret apply: cleanup restores step 5" \
	". '$crit/lib.sh'; crit_init; pull_secret_up; sleep 20"
rm -f "$tmp/kill-on-secret"
restore_after "public images: deleting the secret arms the restore" \
	". '$crit/lib.sh'; crit_init; images_public() { return 0; }; pull_secret_up; exit 0"
[ "$fails" = 0 ] && { echo "PASS test-lib-no-side-effects"; exit 0; }
echo "FAIL test-lib-no-side-effects ($fails)"
exit 1
