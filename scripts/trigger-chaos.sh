#!/usr/bin/env bash
set -euo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
TARGET_SERVICE=""
CHAOS_TYPE=""
INTENSITY="${2:-medium}"
DURATION="${3:-30}"

usage() {
    cat <<USAGE
Usage: $(basename "$0") <type> [intensity] [duration_seconds] [--service <name>]

Trigger chaos scenarios against the loadgen microservices.

Types:
  cpu             Spike CPU usage
  memory          Spike memory allocation
  errors          Inject random HTTP errors
  latency         Add artificial latency to responses
  logstorm        Generate a flood of log entries
  db-slow         Simulate slow database queries
  queue-backlog   Create a NATS queue backlog
  pod-crash       Simulate a service crash/restart

Options:
  intensity       low, medium, high (default: medium)
  duration        Duration in seconds (default: 30)
  --service NAME  Target a specific service (default: all services)

Examples:
  $(basename "$0") latency high 60
  $(basename "$0") errors medium 30 --service order-service
  $(basename "$0") cpu low 15 --service payment-service
USAGE
    exit 1
}

# Parse arguments
if [[ $# -lt 1 ]]; then
    usage
fi

CHAOS_TYPE="$1"
shift

POSITIONAL=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --service)
            TARGET_SERVICE="$2"
            shift 2
            ;;
        *)
            POSITIONAL+=("$1")
            shift
            ;;
    esac
done

if [[ ${#POSITIONAL[@]} -ge 1 ]]; then
    INTENSITY="${POSITIONAL[0]}"
fi
if [[ ${#POSITIONAL[@]} -ge 2 ]]; then
    DURATION="${POSITIONAL[1]}"
fi

# Map intensity to numeric values
case "$INTENSITY" in
    low)    intensity_value=25  ;;
    medium) intensity_value=50  ;;
    high)   intensity_value=90  ;;
    *)      intensity_value=50  ;;
esac

SERVICES=("auth-service" "user-service" "order-service" "payment-service" "notification-worker" "gateway")

if [[ -n "$TARGET_SERVICE" ]]; then
    SERVICES=("$TARGET_SERVICE")
fi

trigger_chaos() {
    local service="$1"
    local chaos_type="$2"
    local intensity="$3"
    local duration="$4"

    # Resolve service URL: gateway is at the gateway, others are routed through it
    local url="${GATEWAY_URL}/chaos"

    local payload
    payload=$(cat <<JSON
{
  "type": "${chaos_type}",
  "target_service": "${service}",
  "intensity": ${intensity},
  "duration_seconds": ${duration}
}
JSON
)

    echo "  -> Triggering ${chaos_type} on ${service} (intensity=${intensity}, duration=${duration}s)"
    response=$(curl -s -w "\n%{http_code}" -X POST \
        -H "Content-Type: application/json" \
        -d "$payload" \
        "$url" 2>&1) || true

    http_code=$(echo "$response" | tail -1)
    body=$(echo "$response" | sed '$d')

    if [[ "$http_code" == "200" || "$http_code" == "202" ]]; then
        echo "     OK (HTTP ${http_code}): ${body}"
    else
        echo "     WARN (HTTP ${http_code}): ${body}"
    fi
}

echo "============================================"
echo "  Chaos Trigger: ${CHAOS_TYPE}"
echo "  Intensity:     ${INTENSITY} (${intensity_value})"
echo "  Duration:      ${DURATION}s"
echo "  Target:        ${TARGET_SERVICE:-all services}"
echo "============================================"
echo ""

for svc in "${SERVICES[@]}"; do
    trigger_chaos "$svc" "$CHAOS_TYPE" "$intensity_value" "$DURATION"
done

echo ""
echo "==> Checking chaos status..."
sleep 2

status_response=$(curl -s "${GATEWAY_URL}/chaos/status" 2>&1) || true
if [[ -n "$status_response" ]]; then
    echo "$status_response" | python3 -m json.tool 2>/dev/null || echo "$status_response"
else
    echo "  Could not retrieve chaos status (gateway may not expose /chaos/status)"
fi

echo ""
echo "Done. Chaos will automatically stop after ${DURATION}s."
