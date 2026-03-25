#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAMESPACE='loadgen'
CHAOS_TYPE=''
INTENSITY='${2:-medium}'
DURATION='${3:-30}'

usage() {
    cat <<USAGE
Usage: $(basename "$0") <type> [intensity] [duration_seconds] [--service <name>]

Trigger chaos scenarios against loadgen services.

Types:
  latency    Add artificial latency to responses
  errors     Inject random HTTP errors
  cpu        Spike CPU usage
  memory     Spike memory allocation

Options:
  intensity  low, medium, high (default: medium)
  duration   Duration in seconds (default: 30)
  --service  Target a specific service (default: all services)
USAGE
    exit 1
}

if [[ $# -lt 1 ]]; then
    usage
fi

CHAOS_TYPE="$1"
shift

TARGET_SERVICE=''
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

case "$INTENSITY" in
    low)    intensity_value=25  ;;
    medium) intensity_value=50  ;;
    high)   intensity_value=90  ;;
    *)      intensity_value=50  ;;
esac

SERVICES=('auth-service' 'user-service' 'order-service' 'payment-service' 'notification-worker' 'gateway')

if [[ -n "$TARGET_SERVICE" ]]; then
    SERVICES=("$TARGET_SERVICE")
fi

trigger_chaos() {
    local service="$1"
    local chaos_type="$2"
    local intensity="$3"
    local duration="$4"

    local url="http://${service}.${SERVICE_NAMESPACE}.svc.cluster.local:8080/chaos"

    local payload=$(cat <<JSON
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

echo '============================================'
echo "  Chaos Trigger: ${CHAOS_TYPE}"
echo "  Intensity:     ${INTENSITY} (${intensity_value})"
echo "  Duration:      ${DURATION}s"
echo "  Target:        ${TARGET_SERVICE:-all services}"
echo "  Namespace:     ${SERVICE_NAMESPACE}"
echo '============================================'
echo ''

for svc in "${SERVICES[@]}"; do
    trigger_chaos "$svc" "$CHAOS_TYPE" "$intensity_value" "$DURATION"
done

echo ''
echo "Done. Chaos will be injected for ${DURATION}s."
