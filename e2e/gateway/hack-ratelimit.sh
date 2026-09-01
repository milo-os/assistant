#!/usr/bin/env bash
# hack-ratelimit.sh — STRETCH (CONTRACT-GATEWAY.md §5). Enable/disable the
# Envoy Gateway global rate-limit backend (Redis) so the token-budget
# BackendTrafficPolicy (manifests/70-...) can enforce a 429. Kept separate
# from up.sh because it's a further EG override (Redis + rate-limit config).
#
# Usage: hack-ratelimit.sh up | down
# Mirrors AI Gateway v1.0.0 examples/token_ratelimit/{redis.yaml,envoy-gateway-values-addon.yaml}.
set -euo pipefail

TEST_INFRA_DIR="${TEST_INFRA_DIR:-/Users/scotwells/repos/datum-cloud/test-infra}"
EG_NS="${EG_NS:-envoy-gateway-system}"
KCFG="$TEST_INFRA_DIR/kubeconfig"
kc() { kubectl --kubeconfig "$KCFG" "$@"; }

REDIS_MANIFEST='
apiVersion: v1
kind: Namespace
metadata: { name: redis-system }
---
apiVersion: v1
kind: Service
metadata: { name: redis, namespace: redis-system, labels: { app: redis } }
spec:
  ports: [{ name: redis, port: 6379 }]
  selector: { app: redis }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: redis, namespace: redis-system }
spec:
  replicas: 1
  selector: { matchLabels: { app: redis } }
  template:
    metadata: { labels: { app: redis } }
    spec:
      containers:
        - name: redis
          image: redis:8.2.4-alpine3.22
          imagePullPolicy: IfNotPresent
          ports: [{ name: redis, containerPort: 6379 }]
'

RATELIMIT_PATCH='{"spec":{"values":{"config":{"envoyGateway":{
  "provider":{"kubernetes":{"rateLimitDeployment":{"patch":{"type":"StrategicMerge","value":{"spec":{"template":{"spec":{"containers":[{"name":"envoy-ratelimit","imagePullPolicy":"IfNotPresent","image":"docker.io/envoyproxy/ratelimit:60d8e81b"}]}}}}}}}},
  "rateLimit":{"backend":{"type":"Redis","redis":{"url":"redis.redis-system.svc.cluster.local:6379"}}}
}}}}}'

case "${1:-}" in
  up)
    echo "$REDIS_MANIFEST" | kc apply -f -
    kc -n redis-system rollout status deploy/redis --timeout=120s
    kc -n flux-system patch helmrelease envoy-gateway --type merge -p "$RATELIMIT_PATCH"
    kc -n flux-system annotate helmrelease envoy-gateway \
      "reconcile.fluxcd.io/requestedAt=$(date +%s)" --overwrite >/dev/null
    for i in $(seq 1 60); do
      kc -n "$EG_NS" get cm envoy-gateway-config -o yaml 2>/dev/null | grep -q 'rateLimit' && break
      [ "$i" = 60 ] && { echo "timed out waiting for rateLimit in EG config" >&2; exit 1; }
      sleep 2
    done
    kc -n "$EG_NS" rollout restart deploy/envoy-gateway
    kc -n "$EG_NS" rollout status deploy/envoy-gateway --timeout=180s
    echo "rate-limit backend (Redis) enabled"
    ;;
  down)
    kc -n flux-system patch helmrelease envoy-gateway --type merge \
      -p '{"spec":{"values":{"config":{"envoyGateway":{"rateLimit":null}}}}}' 2>/dev/null || true
    kc delete namespace redis-system --ignore-not-found --wait=false 2>/dev/null || true
    ;;
  *)
    echo "usage: $0 up|down" >&2; exit 2 ;;
esac
