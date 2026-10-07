#!/usr/bin/env bash
# Layer 3 (contracts/testing-strategy.md): deploy the real chart into a throwaway kind cluster and
# check that its BoothModule is accepted by booth-core's real CRD schema, that it reads the
# Secret core provisions for a module declaring `database`, and that it becomes ready against a
# real PostgreSQL. Used by .github/workflows/integration.yml and runnable locally.
#
# What stands in for booth-core (and why): the BoothModule CRD is booth-core's real one (vendored in
# test/integration/fixtures from booth-core's chart, so the manifest is validated by the real
# schema). Core's ADR 0053 provisioning is played by this script: a postgres:16-alpine Pod (the
# image core bundles) and a booth-database-credentials Secret in the shape core-platform-api.md
# documents. Running real core means its OIDC provider, controller and a real identity, which this
# scaffold has no use for yet; once the module verifies tokens and calls the broker, this script
# gains a real core, the way booth-logging's `real-core` job pins one by SHA.
#
# KEEP_CLUSTER=1 leaves the cluster up for poking at.
set -euo pipefail

CLUSTER=${CLUSTER:-booth-api-it}
NS=booth-api
PGPASS=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
cd "$(dirname "$0")/.."

cleanup() { [ "${KEEP_CLUSTER:-}" = 1 ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --wait 120s
fi

docker build -t booth-api:it .
kind load docker-image booth-api:it --name "$CLUSTER"

kubectl apply -f test/integration/fixtures/boothmodule-crd.yaml
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -

pg_up() {
kubectl -n "$NS" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: pg
  labels: {app: pg}
spec:
  containers:
    - name: postgres
      image: postgres:16-alpine
      env:
        - {name: POSTGRES_USER, value: booth_mod_api}
        - {name: POSTGRES_PASSWORD, value: "$PGPASS"}
        - {name: POSTGRES_DB, value: booth_mod_api}
      readinessProbe:
        exec: {command: ["pg_isready", "-U", "booth_mod_api", "-d", "booth_mod_api"]}
        periodSeconds: 2
---
apiVersion: v1
kind: Service
metadata:
  name: pg
spec:
  selector: {app: pg}
  ports: [{port: 5432}]
EOF
kubectl -n "$NS" wait --for=condition=Ready pod/pg --timeout=180s
}
echo "--- stand-in for booth-core's bundled PostgreSQL and its ADR 0053 Secret"
pg_up
DSN="postgres://booth_mod_api:$PGPASS@pg.$NS.svc.cluster.local:5432/booth_mod_api?sslmode=disable"
kubectl -n "$NS" create secret generic booth-database-credentials \
  --from-literal=dsn="$DSN" --from-literal=host="pg.$NS.svc.cluster.local" --from-literal=port=5432 \
  --from-literal=database=booth_mod_api --from-literal=username=booth_mod_api --from-literal=password="$PGPASS" \
  --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install a charts/booth-api -n "$NS" \
  --set image.repository=booth-api --set image.tag=it --set image.pullPolicy=Never --wait --timeout 5m

echo "--- BoothModule accepted by booth-core's real CRD schema:"
kubectl -n "$NS" get boothmodule api -o jsonpath='{.spec.id} navPath={.spec.navPath} database={.spec.database.enabled}{"\n"}'

echo "--- readiness against the real database (helm --wait already required it):"
kubectl -n "$NS" get deploy a-booth-api -o jsonpath='{.status.readyReplicas}/{.spec.replicas} ready{"\n"}'

echo "--- /healthz goes unready when the database goes away, and recovers:"
pod=$(kubectl -n "$NS" get pod -l app.kubernetes.io/component=api -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$NS" delete pod pg --wait
kubectl -n "$NS" wait --for=condition=Ready=false pod/"$pod" --timeout=60s
echo "unready with the database gone: yes"
# The pod must not have restarted: /livez does not depend on the database.
restarts=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}')
[ "$restarts" = 0 ] || { echo "module restarted $restarts times while the database was down"; exit 1; }
echo "no restarts while the database was down: yes"
pg_up
kubectl -n "$NS" wait --for=condition=Ready pod/"$pod" --timeout=90s
echo "ready again once the database is back: yes"

echo "kind integration: OK"
