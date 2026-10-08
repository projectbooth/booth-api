#!/usr/bin/env bash
# Real-stack integration test (contracts/testing-strategy.md layer 3, against real counterparts):
# Keycloak, booth-core, booth-database, booth-catalog and this booth-api, each built from source and
# installed from its own chart on a kind cluster, then test/integration/realstack/check run as a Job
# inside it. Nothing is stubbed: tokens come from Keycloak, the catalog is reached through core's
# gateway, workspace data through core's broker, booth-database's provider and booth-core's real
# credential sidecar, and keys are used on core's real public route.
#
#   CORE_DIR=../booth-core DATABASE_DIR=../booth-database CATALOG_DIR=../booth-catalog \
#     bash hack/realstack-integration.sh
#
# CI checks each sibling out at a pinned commit (.github/workflows/integration.yml). Install values
# follow booth-e2e's bringup.sh and booth-logging's real-core job. KEEP_CLUSTER=1 leaves it up.
set -euo pipefail

CLUSTER=${CLUSTER:-booth-api-realstack}
CORE_DIR=${CORE_DIR:-../booth-core}
DATABASE_DIR=${DATABASE_DIR:-../booth-database}
CATALOG_DIR=${CATALOG_DIR:-../booth-catalog}
cd "$(dirname "$0")/.."
HERE=test/integration/realstack
ISSUER=http://keycloak.keycloak.svc:8080/realms/booth
CORE_URL=http://booth-core.booth-system.svc:8080

cleanup() { [ "${KEEP_CLUSTER:-}" = 1 ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
trap cleanup EXIT
dump() {
  echo "=== state on failure"
  kubectl get pods -A -o wide || true
  kubectl get boothmodules -A -o wide || true
  for d in booth-system/booth-core booth-api/a-booth-api booth-database/db-booth-database booth-catalog/booth-catalog; do
    echo "--- logs $d"; kubectl -n "${d%%/*}" logs "deploy/${d##*/}" --tail=80 || true
  done
  kubectl -n realstack-check logs job/check --tail=200 || true
}

for d in "$CORE_DIR" "$DATABASE_DIR" "$CATALOG_DIR"; do
  [ -d "$d" ] || { echo "missing checkout: $d"; exit 1; }
  echo "using $d at $(git -C "$d" rev-parse --short HEAD 2>/dev/null || echo '?')"
done

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s

echo "--- images"
helm repo add nats https://nats-io.github.io/k8s/helm/charts/ >/dev/null 2>&1 || true
helm dependency build "$CORE_DIR/charts/booth-core" >/dev/null
docker build -q -t booth-core:rs "$CORE_DIR" >/dev/null
docker build -q -t booth-database:rs "$DATABASE_DIR" >/dev/null
docker build -q -t booth-catalog:rs "$CATALOG_DIR" >/dev/null
docker build -q -t booth-api:rs . >/dev/null
docker build -q -t booth-api-check:rs -f "$HERE/Dockerfile" . >/dev/null
kind load docker-image booth-core:rs booth-database:rs booth-catalog:rs booth-api:rs booth-api-check:rs --name "$CLUSTER"

trap 'dump' ERR

echo "--- Keycloak (alice: acme owner; bob, dave: acme editors; carol: globex owner)"
PASSWORD=$(openssl rand -hex 12)
ADMIN_PASSWORD=$(openssl rand -hex 12)
kubectl create namespace keycloak --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n keycloak create secret generic keycloak-admin --from-literal=password="$ADMIN_PASSWORD" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
sed "s/__TEST_PASSWORD__/$PASSWORD/g" "$HERE/realm-booth.json.tpl" > "$HERE/.realm.json"
kubectl -n keycloak create configmap keycloak-realm --from-file=realm-booth.json="$HERE/.realm.json" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
rm -f "$HERE/.realm.json"
kubectl apply -f "$HERE/keycloak.yaml" >/dev/null
kubectl -n keycloak rollout restart deploy/keycloak >/dev/null 2>&1 || true
kubectl -n keycloak rollout status deploy/keycloak --timeout=600s

echo "--- booth-core"
kubectl create namespace booth-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f "$CORE_DIR/charts/booth-core/crds/" >/dev/null # helm never updates an existing CRD
helm upgrade --install booth-core "$CORE_DIR/charts/booth-core" -n booth-system \
  --set oidc.issuerUrl=$ISSUER --set oidc.clientId=booth-design \
  --set workloadIdentity.issuerUrl=$CORE_URL \
  --set-string iframeSigningKey="$(openssl rand -hex 32)" \
  --set image.repository=booth-core --set image.tag=rs --set image.pullPolicy=Never --wait --timeout 10m >/dev/null
kubectl -n booth-system rollout status deploy/booth-core --timeout=300s

echo "--- booth-database (bundled PostgreSQL, postgres-kind credential provider)"
kubectl create namespace booth-database --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm upgrade --install db "$DATABASE_DIR/charts/booth-database" -n booth-database \
  --set image.repository=booth-database --set image.tag=rs --set image.pullPolicy=Never --wait --timeout 10m >/dev/null

echo "--- booth-catalog (its own small PostgreSQL: its chart takes a DSN Secret, as in booth-e2e)"
kubectl create namespace booth-catalog --dry-run=client -o yaml | kubectl apply -f - >/dev/null
CATPW=$(openssl rand -hex 12)
kubectl -n booth-catalog apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: pg, labels: {app: catalog-pg}}
spec:
  containers:
    - name: postgres
      image: postgres:16-alpine
      env:
        - {name: POSTGRES_USER, value: catalog}
        - {name: POSTGRES_PASSWORD, value: "$CATPW"}
        - {name: POSTGRES_DB, value: catalog}
      readinessProbe: {exec: {command: ["pg_isready", "-U", "catalog"]}, periodSeconds: 2}
---
apiVersion: v1
kind: Service
metadata: {name: catalog-pg}
spec: {selector: {app: catalog-pg}, ports: [{port: 5432}]}
EOF
kubectl -n booth-catalog create secret generic catalog-db \
  --from-literal=dsn="postgres://catalog:$CATPW@catalog-pg.booth-catalog.svc:5432/catalog?sslmode=disable" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n booth-catalog wait --for=condition=Ready pod/pg --timeout=180s >/dev/null
helm upgrade --install booth-catalog "$CATALOG_DIR/charts/booth-catalog" -n booth-catalog \
  --set oidc.issuerUrl=$ISSUER --set oidc.clientId=booth-design --set oidc.groupsClaim=groups \
  --set nats.url=nats://booth-core-nats.booth-system.svc:4222 --set postgres.dsnSecret.name=catalog-db \
  --set image.repository=booth-catalog --set image.tag=rs --set image.pullPolicy=Never --wait --timeout 10m >/dev/null

echo "--- booth-api (data access on: core.url set)"
kubectl create namespace booth-api --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm upgrade --install a charts/booth-api -n booth-api \
  --set core.url=$CORE_URL --set oidc.issuerUrl=$ISSUER --set oidc.clientId=booth-design \
  --set image.repository=booth-api --set image.tag=rs --set image.pullPolicy=Never --wait --timeout 10m >/dev/null
kubectl get boothmodule -A -o custom-columns=NS:.metadata.namespace,ID:.spec.id,PUBLIC:.spec.publicRoutes.pathPrefixes,MINT:.spec.workloadIdentity.mint

echo "--- check (in-cluster, in a namespace booth-database's NetworkPolicy admits, to seed tables)"
kubectl create namespace realstack-check --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl label namespace realstack-check booth.projectbooth.io/database-client=true --overwrite >/dev/null
kubectl -n realstack-check delete job check --ignore-not-found >/dev/null
kubectl -n realstack-check apply -f - >/dev/null <<EOF
apiVersion: batch/v1
kind: Job
metadata: {name: check}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: check
          image: booth-api-check:rs
          imagePullPolicy: Never
          env:
            - {name: TEST_PASSWORD, value: "$PASSWORD"}
            - {name: KEYCLOAK_ADMIN_PASSWORD, value: "$ADMIN_PASSWORD"}
EOF
if ! kubectl -n realstack-check wait --for=condition=complete job/check --timeout=600s; then
  kubectl -n realstack-check logs job/check || true
  dump
  exit 1
fi
kubectl -n realstack-check logs job/check
echo "--- booth-api's own log (sidecar lifecycle)"
kubectl -n booth-api logs deploy/a-booth-api | grep -E "sidecar|data access" | head -20
echo "realstack integration: OK"
