#!/usr/bin/env bash
# Registers the loadgen host's OWN observability stack (Prometheus, Loki,
# Jaeger — already running per docker-compose/k8s manifests) as InfraSage
# federated sources, turning this host into a BYO-stack parity testbed:
# run the same verdict scenarios against a federated tenant and compare
# scorecards with the ingest tenant.
#
# Requires: INFRASAGE_BASE_URL, INFRASAGE_TOKEN (admin), and the stack
# reachable FROM the InfraSage cluster (adjust *_URL to routable addrs).
set -euo pipefail
: "${INFRASAGE_BASE_URL:?set INFRASAGE_BASE_URL}"
: "${INFRASAGE_TOKEN:?set INFRASAGE_TOKEN (admin role)}"
PROM_URL="${PROM_URL:-http://loadgen-host:9090}"
LOKI_URL="${LOKI_URL:-http://loadgen-host:3100}"
JAEGER_URL="${JAEGER_URL:-http://loadgen-host:16686}"

auth=(-H "Authorization: Bearer $INFRASAGE_TOKEN" -H "Content-Type: application/json")
[ -n "${INFRASAGE_TENANT:-}" ] && auth+=(-H "X-Tenant-ID: $INFRASAGE_TENANT")

echo "→ prometheus (aggregate-pull detection)"
curl -sf "${auth[@]}" -X POST "$INFRASAGE_BASE_URL/api/v1/federated/sources" -d @- <<JSON
{"provider":"prometheus","name":"loadgen-prom","config":{
  "base_url":"$PROM_URL","environment":"loadgen",
  "queries":[
    {"name":"http_request_rate","query":"sum by (job) (rate(http_requests_total[2m]))","service_label":"job"},
    {"name":"http_error_rate","query":"sum by (job) (rate(http_requests_total{status=~\"5..\"}[2m]))","service_label":"job"},
    {"name":"latency_p99_ms","query":"histogram_quantile(0.99, sum by (le, job) (rate(http_request_duration_seconds_bucket[2m]))) * 1000","service_label":"job"}
  ]}}
JSON
echo; echo "→ loki (question-time log evidence)"
curl -sf "${auth[@]}" -X POST "$INFRASAGE_BASE_URL/api/v1/federated/sources" \
  -d "{\"provider\":\"loki\",\"name\":\"loadgen-loki\",\"config\":{\"base_url\":\"$LOKI_URL\",\"service_label\":\"service\"}}"
echo; echo "→ jaeger (question-time trace evidence)"
curl -sf "${auth[@]}" -X POST "$INFRASAGE_BASE_URL/api/v1/federated/sources" \
  -d "{\"provider\":\"jaeger\",\"name\":\"loadgen-jaeger\",\"config\":{\"base_url\":\"$JAEGER_URL\"}}"
echo; echo "Done. Ensure FEDERATED_TELEMETRY_ENABLED=true on the engine; sources reconcile within 60s."
echo "Then: verdict suite --tags detection --report-dir ./reports/federated"
