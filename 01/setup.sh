#!/usr/bin/env bash
# Deploy Koupa step 0 in stages (design 3.5). Safe to run again.
#
#   0. label the infra worker (INFRA_NODE, default: the first worker by
#      name) koupa.dev/infra=true; the label is removed from other nodes
#   1. CNPG operator (its controller pinned to the infra worker), then the
#      koupa-db Cluster (pinned there too); wait until it is Ready
#   2. migration Job (old one deleted first); stop and print logs on failure
#   3. app, Service, Gateway, HTTPRoute (k8s/base); wait for the rollout
#      and for the Gateway to be Programmed, then print its address
#
# Uses the current kubectl context. The database and the operator live on
# the infra worker so another worker can be drained without
# --delete-emptydir-data (plan A15; measure.sh drain never picks it).
set -euo pipefail
cd "$(dirname "$0")"

CNPG_VERSION=1.30.1
CNPG_MANIFEST=https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v${CNPG_VERSION}/cnpg-${CNPG_VERSION}.yaml
NS=koupa
INFRA_LABEL=koupa.dev/infra

log() { printf '\n==> %s\n' "$*"; }
die() { echo "setup.sh: $*" >&2; exit 1; }

command -v kubectl >/dev/null || die "kubectl not found"
command -v python3 >/dev/null || die "python3 not found"
log "context: $(kubectl config current-context)"

log "checking the step-0 manifest"
scripts/check-manifests.sh base >/dev/null || { scripts/check-manifests.sh base | grep -v '^ok' >&2; die "k8s/base fails its checks"; }

# --- 0. infra worker ---------------------------------------------------------
workers=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort)
INFRA_NODE=${INFRA_NODE:-$(echo "$workers" | head -1)}
echo "$workers" | grep -qx "$INFRA_NODE" || die "INFRA_NODE=$INFRA_NODE is not a worker (workers: $(echo $workers))"
log "0/3 infra worker $INFRA_NODE ($INFRA_LABEL=true): database and CNPG operator"
# A local-path volume with node affinity cannot move: refuse a pin
# elsewhere, before any label changes. (tk8s' local-path uses a shared host
# directory and sets no node affinity; then there is nothing to check.)
pv=$(kubectl -n "$NS" get pvc koupa-db-1 -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
if [ -n "$pv" ]; then
  pv_node=$(kubectl get pv "$pv" -o jsonpath='{.spec.nodeAffinity.required.nodeSelectorTerms[0].matchExpressions[0].values[0]}' 2>/dev/null || true)
  [ -z "$pv_node" ] || [ "$pv_node" = "$INFRA_NODE" ] ||
    die "the koupa-db volume $pv is on $pv_node, not INFRA_NODE=$INFRA_NODE (local-path); use INFRA_NODE=$pv_node"
fi
for n in $(kubectl get nodes -l "$INFRA_LABEL=true" -o jsonpath='{.items[*].metadata.name}'); do
  [ "$n" = "$INFRA_NODE" ] || kubectl label node "$n" "$INFRA_LABEL-"
done
kubectl label node "$INFRA_NODE" "$INFRA_LABEL=true" --overwrite

# --- 1. database --------------------------------------------------------------
log "1/3 CloudNativePG operator v${CNPG_VERSION}"
kubectl apply --server-side --force-conflicts -f "$CNPG_MANIFEST" >/dev/null
# Pin the controller (it has an emptyDir) to the infra worker.
kubectl -n cnpg-system patch deployment cnpg-controller-manager --type merge \
  -p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"$INFRA_LABEL\":\"true\"}}}}}"
kubectl -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=300s

log "1/3 PostgreSQL Cluster koupa-db"
kubectl apply -f k8s/base/namespace.yaml
# The operator's webhook may need a few seconds after the rollout to answer.
for i in $(seq 1 30); do
  if out=$(kubectl apply -f k8s/db/cluster.yaml 2>&1); then
    echo "$out"
    break
  fi
  [ "$i" -eq 30 ] && { echo "$out" >&2; die "could not apply k8s/db/cluster.yaml"; }
  echo "waiting for the CNPG webhook ($i/30): ${out##*: }"
  sleep 5
done
kubectl -n "$NS" wait cluster/koupa-db --for=condition=Ready --timeout=600s
# A changed spec (e.g. the infra pin) restarts the instance a moment after
# the apply, while Ready may still read True: require a healthy phase and
# Ready on three reads 5 s apart.
ok=0
for i in $(seq 1 120); do
  st=$(kubectl -n "$NS" get cluster koupa-db -o jsonpath='{.status.phase}|{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  if [ "$st" = "Cluster in healthy state|True" ]; then ok=$((ok + 1)); else ok=0; fi
  [ "$ok" -ge 3 ] && break
  [ "$i" -eq 120 ] && die "koupa-db not healthy after 600 s ($st)"
  sleep 5
done
kubectl -n "$NS" get secret koupa-db-app >/dev/null || die "secret koupa-db-app missing"

# --- 2. migrations ------------------------------------------------------------
log "2/3 migration Job"
migrate=$(kubectl kustomize --load-restrictor LoadRestrictionsNone k8s/migrate)
latest=$(ls migrations | sed -n 's/^\([0-9]\{6\}\)_.*\.up\.sql$/\1/p' | sort | tail -1)
job=koupa-migrate-$latest
# Guard: every file in migrations/ is in the ConfigMap and the Job carries
# the latest number. Otherwise k8s/migrate is out of date.
printf '%s\n' "$migrate" | python3 -I -c '
import os, sys, yaml
job, mdir = sys.argv[1], sys.argv[2]
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
cms = [d for d in docs if d["kind"] == "ConfigMap"]
jobs = [d["metadata"]["name"] for d in docs if d["kind"] == "Job"]
want = sorted(f for f in os.listdir(mdir) if f.endswith(".sql"))
have = sorted(cms[0]["data"]) if len(cms) == 1 else []
if jobs != [job] or cms[0]["metadata"]["name"] != job or want != have:
    sys.exit("k8s/migrate is out of date: want Job/ConfigMap %s with %s, got Jobs %s with %s" % (job, want, jobs, have))
' "$job" migrations || die "update k8s/migrate/kustomization.yaml and job.yaml"

kubectl -n "$NS" delete job "$job" --ignore-not-found --wait=true
printf '%s\n' "$migrate" | kubectl apply -f -

status=""
for i in $(seq 1 120); do
  status=$(kubectl -n "$NS" get job "$job" -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}{" "}{end}')
  case " $status " in
    *" Complete "*) break ;;
    *" Failed "*) break ;;
  esac
  sleep 2
done
if [[ " $status " != *" Complete "* ]]; then
  echo "migration Job $job did not complete (conditions: ${status:-none}); logs:" >&2
  kubectl -n "$NS" logs -l "job-name=$job" --all-containers --prefix --tail=-1 >&2 || true
  kubectl -n "$NS" describe job "$job" | tail -20 >&2 || true
  die "migration failed"
fi
kubectl -n "$NS" logs -l "job-name=$job" --tail=-1 || true
echo "job/$job complete"

# --- 3. app -------------------------------------------------------------------
log "3/3 app, Service, Gateway, HTTPRoute (step 0)"
kubectl apply -k k8s/base
kubectl -n "$NS" rollout status deployment/koupa --timeout=300s
kubectl -n "$NS" wait gateway/koupa --for=condition=Programmed --timeout=300s

addr=$(kubectl -n "$NS" get gateway koupa -o jsonpath='{.status.addresses[0].value}')
[ -n "$addr" ] || die "Gateway koupa has no address"
kubectl -n "$NS" get pods -o wide -l app.kubernetes.io/name=koupa
log "Gateway address: $addr"
echo "try: curl http://$addr/api/whoami"
