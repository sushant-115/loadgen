#!/usr/bin/env bash
# start-demo.sh — start the loadgen demo stack locally via docker-compose.
#
# Prerequisites:
#   - Docker Desktop running
#   - INFRASAGE_API_KEY exported (or .env file in loadgen root)
#
# Usage:
#   cd /path/to/loadgen
#   INFRASAGE_API_KEY=isage_... ./scripts/start-demo.sh
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOADGEN_DIR="$(dirname "${SCRIPT_DIR}")"

cd "${LOADGEN_DIR}"

INFRASAGE_API_KEY="${INFRASAGE_API_KEY:-isage_13dbf06f8384cafaf308798a6a2e155ba4d104bc}"
export INFRASAGE_API_KEY

INFRASAGE_EMAIL="${INFRASAGE_EMAIL:-admin@infrasage.local}"
INFRASAGE_PASSWORD="${INFRASAGE_PASSWORD:-InfraSage@2026!}"
INFRASAGE_API_URL="${INFRASAGE_API_URL:-https://api.infrasage.dev}"

echo "========================================================"
echo "  InfraSage Demo — loadgen local startup"
echo "========================================================"
echo "  INFRASAGE_API_URL  = ${INFRASAGE_API_URL}"
echo "  INFRASAGE_API_KEY  = ${INFRASAGE_API_KEY:0:20}..."
echo ""

# 1. Verify Docker is running
if ! docker info &>/dev/null; then
  echo "ERROR: Docker daemon not running. Start Docker Desktop first."
  exit 1
fi

# 2. Pull / build image if needed
if ! docker image inspect loadgen:latest &>/dev/null; then
  echo "Building loadgen:latest image..."
  docker build -f Dockerfile.multi -t loadgen:latest .
  echo "Build complete."
fi

# 3. Start the stack (detached)
echo "Starting docker-compose stack..."
INFRASAGE_API_KEY="${INFRASAGE_API_KEY}" \
  docker compose -f docker-compose.yml -f docker-compose.override.yml up -d

echo ""
echo "Waiting for services to become healthy..."

# 4. Wait for gateway to respond (up to 90s)
max_wait=90
elapsed=0
while true; do
  if curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/health 2>/dev/null | grep -qE "^(200|404)"; then
    echo "  ✓ gateway is up"
    break
  fi
  if [[ $elapsed -ge $max_wait ]]; then
    echo "  ✗ gateway did not respond after ${max_wait}s — check: docker compose logs gateway"
    exit 1
  fi
  sleep 5
  elapsed=$((elapsed + 5))
  echo "  ...waiting (${elapsed}s)"
done

# 5. Wait for auth-service
elapsed=0
while true; do
  if curl -s -o /dev/null -w "%{http_code}" http://localhost:8081/health 2>/dev/null | grep -qE "^(200|404)"; then
    echo "  ✓ auth-service is up"
    break
  fi
  if [[ $elapsed -ge $max_wait ]]; then
    echo "  ✗ auth-service did not respond — check: docker compose logs auth-service"
    break
  fi
  sleep 5
  elapsed=$((elapsed + 5))
done

echo ""
echo "=== Container status ==="
docker ps --format "table {{.Names}}\t{{.Status}}" | sort

echo ""
echo "Waiting 30s for OTEL telemetry to start flowing to InfraSage..."
sleep 30

echo ""
echo "=== InfraSage vectorizer check ==="
TOKEN=$(curl -s -X POST "${INFRASAGE_API_URL}/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"${INFRASAGE_EMAIL}\",\"password\":\"${INFRASAGE_PASSWORD}\"}" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('token',''))")

if [[ -z "${TOKEN}" ]]; then
  echo "  WARNING: Could not obtain InfraSage auth token (API may be unavailable)"
else
  curl -s -H "Authorization: Bearer ${TOKEN}" "${INFRASAGE_API_URL}/api/v1/vectorizer/stats" \
    | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(f'  services_indexed: {d.get(\"services_indexed\",0)}')
print(f'  total_embeddings: {d.get(\"total_embeddings\",0)}')
print(f'  running:          {d.get(\"running\",False)}')
"
fi

echo ""
echo "========================================================"
echo "  Stack is READY. Let telemetry accumulate for ~30 min"
echo "  before running demo injection for best watchdog results."
echo ""
echo "  Demo commands:"
echo "    ./scripts/demo-inject.sh inject auth_service_degradation 300"
echo "    ./scripts/demo-inject.sh watchdog"
echo "    ./scripts/demo-inject.sh clear"
echo "    ./scripts/demo-inject.sh inject retry_amplification 600"
echo "    ./scripts/demo-inject.sh ciad-scores"
echo "    ./scripts/demo-inject.sh inject queue_consumer_lag 600"
echo ""
echo "  Grafana:  http://localhost:3000  (admin/admin)"
echo "  Jaeger:   http://localhost:16686"
echo "========================================================"
