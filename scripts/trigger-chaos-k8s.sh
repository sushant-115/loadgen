#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAMESPACE='loadgen'
CHAOS_TYPE=''
INTENSITY='medium'
DURATION='30'
TARGET_SERVICE=''
MODE='single'
CAMPAIGN_FILE=''

usage() {
    cat <<USAGE
Usage:
    $(basename "$0") <type> [intensity] [duration_seconds] [--service <name>]
    $(basename "$0") campaign --file <campaign.json> [--service <name>] [--dry-run]

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
  --file     Campaign JSON file (campaign mode)
  --dry-run  Validate campaign only (campaign mode)
USAGE
    exit 1
}

map_type_to_endpoint() {
    case "$1" in
        cpu|cpu_stress) echo "cpu" ;;
        memory|memory_leak) echo "memory" ;;
        errors|error_injection) echo "errors" ;;
        latency|latency_injection) echo "latency" ;;
        logstorm|log_storm) echo "logstorm" ;;
        db-slow|db_slow) echo "db-slow" ;;
        queue-backlog|queue_backlog) echo "queue-backlog" ;;
        pod-crash|pod_crash) echo "pod-crash" ;;
        *) echo "$1" ;;
    esac
}

intensity_to_ratio() {
    case "$1" in
        low) echo "0.25" ;;
        medium) echo "0.50" ;;
        high) echo "0.90" ;;
        *)
            if [[ "$1" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
                if awk "BEGIN {exit !($1 > 1)}"; then
                    awk "BEGIN {printf \"%.4f\", $1/100.0}"
                else
                    echo "$1"
                fi
            else
                echo "0.50"
            fi
            ;;
    esac
}

if [[ $# -lt 1 ]]; then
    usage
fi

CHAOS_TYPE="$1"
shift

if [[ "$CHAOS_TYPE" == "campaign" ]]; then
    MODE='campaign'
fi

POSITIONAL=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --service)
            TARGET_SERVICE="$2"
            shift 2
            ;;
        --file)
            CAMPAIGN_FILE="$2"
            shift 2
            ;;
        --dry-run)
            MODE='campaign-dry-run'
            shift
            ;;
        *)
            POSITIONAL+=("$1")
            shift
            ;;
    esac
done

if [[ "$MODE" == 'single' ]]; then
    if [[ ${#POSITIONAL[@]} -ge 1 ]]; then
        INTENSITY="${POSITIONAL[0]}"
    fi
    if [[ ${#POSITIONAL[@]} -ge 2 ]]; then
        DURATION="${POSITIONAL[1]}"
    fi
fi

intensity_value="$(intensity_to_ratio "$INTENSITY")"

SERVICES=('auth-service' 'user-service' 'order-service' 'payment-service' 'notification-worker' 'gateway')

if [[ -n "$TARGET_SERVICE" ]]; then
    SERVICES=("$TARGET_SERVICE")
fi

trigger_chaos() {
    local service="$1"
    local chaos_type
    chaos_type="$(map_type_to_endpoint "$2")"
    local intensity="$3"
    local duration="$4"

    local port="8080"
    if [[ "$service" == "notification-worker" ]]; then
        port="8085"
    fi
    local url="http://${service}.${SERVICE_NAMESPACE}.svc.cluster.local:${port}/chaos/${chaos_type}"

    local payload
    payload=$(cat <<JSON
{
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

trigger_campaign() {
    local service="$1"
    local payload="$2"

    local port="8080"
    if [[ "$service" == "notification-worker" ]]; then
        port="8085"
    fi
    local url="http://${service}.${SERVICE_NAMESPACE}.svc.cluster.local:${port}/chaos/campaign"

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

run_campaign() {
    if [[ -z "$CAMPAIGN_FILE" ]]; then
        echo "--file is required in campaign mode"
        exit 1
    fi
    if [[ ! -f "$CAMPAIGN_FILE" ]]; then
        echo "campaign file not found: $CAMPAIGN_FILE"
        exit 1
    fi

    local payload
    payload="$(cat "$CAMPAIGN_FILE")"
    if [[ "$MODE" == 'campaign-dry-run' ]]; then
        payload="$(python3 - <<PY
import json
obj = json.loads(open("$CAMPAIGN_FILE","r",encoding="utf-8").read())
obj["dry_run"] = True
print(json.dumps(obj))
PY
)"
    fi

    echo '============================================'
    echo '  Chaos Campaign (K8s)'
    echo "  File:          ${CAMPAIGN_FILE}"
    echo "  Target:        ${TARGET_SERVICE:-all services}"
    echo "  Namespace:     ${SERVICE_NAMESPACE}"
    echo '============================================'
    echo ''

    for svc in "${SERVICES[@]}"; do
        echo "  -> Triggering campaign on ${svc}"
        trigger_campaign "$svc" "$payload"
    done
    echo ''
    echo 'Done.'
}

if [[ "$MODE" == 'campaign' || "$MODE" == 'campaign-dry-run' ]]; then
    run_campaign
    exit 0
fi

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
