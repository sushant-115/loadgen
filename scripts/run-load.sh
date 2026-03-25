#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

echo "==> Starting all services with docker compose..."
docker compose up -d --build

echo "==> Waiting for services to become healthy..."

services=(postgres redis nats)
for svc in "${services[@]}"; do
    printf "    Waiting for %s..." "$svc"
    until docker compose ps "$svc" --format json 2>/dev/null | grep -q '"healthy"'; do
        printf "."
        sleep 2
    done
    echo " ready"
done

# Wait for HTTP endpoints to respond
endpoints=(
    "gateway:http://localhost:8080/health"
    "grafana:http://localhost:3000/api/health"
    "prometheus:http://localhost:9090/-/ready"
    "jaeger:http://localhost:16686/"
)

for entry in "${endpoints[@]}"; do
    name="${entry%%:*}"
    url="${entry#*:}"
    printf "    Waiting for %s..." "$name"
    until curl -sf "$url" > /dev/null 2>&1; do
        printf "."
        sleep 2
    done
    echo " ready"
done

echo ""
echo "============================================"
echo "  All services are up and running!"
echo "============================================"
echo ""
echo "  Grafana:    http://localhost:3000  (admin/admin)"
echo "  Jaeger:     http://localhost:16686"
echo "  Prometheus: http://localhost:9090"
echo "  Gateway:    http://localhost:8080"
echo "  Nginx:      http://localhost:80"
echo ""
echo "==> Tailing logs from all services (Ctrl+C to stop)..."
docker compose logs -f
