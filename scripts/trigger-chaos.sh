#!/usr/bin/env bash
set -euo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
USE_DOCKER_DNS="${USE_DOCKER_DNS:-false}"
TARGET_SERVICE=""
CHAOS_TYPE=""
INTENSITY="medium"
DURATION="30"
MODE="single"
CAMPAIGN_FILE=""
SCHEDULE_AFTER="0"
ADAPTIVE_CHECK_CMD=""
ADAPTIVE_POLL_SECONDS="10"

usage() {
    cat <<USAGE
Usage:
    $(basename "$0") <type> [intensity] [duration_seconds] [--service <name>]
    $(basename "$0") campaign --file <campaign.json> [--service <name>] [--schedule-after <seconds>] [--adaptive-check <cmd>] [--adaptive-poll <seconds>] [--dry-run]

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
    --dry-run       Validate campaign only (campaign mode)

Campaign mode:
    --file PATH            Campaign JSON file
    --schedule-after SEC   Delay before campaign starts
    --adaptive-check CMD   Shell command polled until exit code 0, then start campaign
    --adaptive-poll SEC    Poll interval for adaptive check (default: 10)

Examples:
  $(basename "$0") latency high 60
  $(basename "$0") errors medium 30 --service order-service
  $(basename "$0") campaign --file configs/chaos-campaign.example.json --schedule-after 120
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

# Parse arguments
if [[ $# -lt 1 ]]; then
    usage
fi

CHAOS_TYPE="$1"
shift

if [[ "$CHAOS_TYPE" == "campaign" ]]; then
    MODE="campaign"
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
        --schedule-after)
            SCHEDULE_AFTER="$2"
            shift 2
            ;;
        --adaptive-check)
            ADAPTIVE_CHECK_CMD="$2"
            shift 2
            ;;
        --adaptive-poll)
            ADAPTIVE_POLL_SECONDS="$2"
            shift 2
            ;;
        --dry-run)
            MODE="campaign-dry-run"
            shift
            ;;
        *)
            POSITIONAL+=("$1")
            shift
            ;;
    esac
done

if [[ "$MODE" == "single" ]]; then
    if [[ ${#POSITIONAL[@]} -ge 1 ]]; then
        INTENSITY="${POSITIONAL[0]}"
    fi
    if [[ ${#POSITIONAL[@]} -ge 2 ]]; then
        DURATION="${POSITIONAL[1]}"
    fi
fi

intensity_value="$(intensity_to_ratio "$INTENSITY")"

SERVICES=("auth-service" "user-service" "order-service" "payment-service" "notification-worker" "gateway")

if [[ -n "$TARGET_SERVICE" ]]; then
    SERVICES=("$TARGET_SERVICE")
fi

trigger_chaos() {
    local service="$1"
    local chaos_type
    chaos_type="$(map_type_to_endpoint "$2")"
    local intensity="$3"
    local duration="$4"

    local url
    if [[ "$service" == "gateway" ]]; then
        url="${GATEWAY_URL}/chaos/${chaos_type}"
    else
        if [[ "$USE_DOCKER_DNS" == "true" ]]; then
            local dns
            dns="${service%-service}"
            url="http://${dns}-service:8080/chaos/${chaos_type}"
            if [[ "$service" == "notification-worker" ]]; then
                url="http://notification-worker:8085/chaos/${chaos_type}"
            fi
        else
            case "$service" in
                auth-service) url="http://localhost:8081/chaos/${chaos_type}" ;;
                user-service) url="http://localhost:8082/chaos/${chaos_type}" ;;
                order-service) url="http://localhost:8083/chaos/${chaos_type}" ;;
                payment-service) url="http://localhost:8084/chaos/${chaos_type}" ;;
                notification-worker) url="http://localhost:8085/chaos/${chaos_type}" ;;
                *) url="${GATEWAY_URL}/chaos/${chaos_type}" ;;
            esac
        fi
    fi

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

trigger_campaign_on_service() {
    local service="$1"
    local body="$2"

    local url
    if [[ "$service" == "gateway" ]]; then
        url="${GATEWAY_URL}/chaos/campaign"
    else
        if [[ "$USE_DOCKER_DNS" == "true" ]]; then
            local dns
            dns="${service%-service}"
            url="http://${dns}-service:8080/chaos/campaign"
            if [[ "$service" == "notification-worker" ]]; then
                url="http://notification-worker:8085/chaos/campaign"
            fi
        else
            case "$service" in
                auth-service) url="http://localhost:8081/chaos/campaign" ;;
                user-service) url="http://localhost:8082/chaos/campaign" ;;
                order-service) url="http://localhost:8083/chaos/campaign" ;;
                payment-service) url="http://localhost:8084/chaos/campaign" ;;
                notification-worker) url="http://localhost:8085/chaos/campaign" ;;
                *) url="${GATEWAY_URL}/chaos/campaign" ;;
            esac
        fi
    fi

    response=$(curl -s -w "\n%{http_code}" -X POST \
        -H "Content-Type: application/json" \
        -d "$body" \
        "$url" 2>&1) || true
    http_code=$(echo "$response" | tail -1)
    body_resp=$(echo "$response" | sed '$d')

    if [[ "$http_code" == "200" || "$http_code" == "202" ]]; then
        echo "     OK (HTTP ${http_code}): ${body_resp}"
    else
        echo "     WARN (HTTP ${http_code}): ${body_resp}"
    fi
}

run_campaign_mode() {
    if [[ -z "$CAMPAIGN_FILE" ]]; then
        echo "--file is required for campaign mode"
        exit 1
    fi
    if [[ ! -f "$CAMPAIGN_FILE" ]]; then
        echo "campaign file not found: $CAMPAIGN_FILE"
        exit 1
    fi

    local payload
    payload="$(cat "$CAMPAIGN_FILE")"
    if [[ "$MODE" == "campaign-dry-run" ]]; then
        payload="$(python3 - <<PY
import json
obj = json.loads(open("$CAMPAIGN_FILE","r",encoding="utf-8").read())
obj["dry_run"] = True
print(json.dumps(obj))
PY
)"
    fi

    if [[ "$SCHEDULE_AFTER" != "0" ]]; then
        echo "Scheduling campaign after ${SCHEDULE_AFTER}s"
        sleep "$SCHEDULE_AFTER"
    fi

    if [[ -n "$ADAPTIVE_CHECK_CMD" ]]; then
        echo "Adaptive mode enabled. Waiting for trigger command to succeed..."
        until bash -lc "$ADAPTIVE_CHECK_CMD"; do
            sleep "$ADAPTIVE_POLL_SECONDS"
        done
        echo "Adaptive trigger condition met. Starting campaign."
    fi

    echo "============================================"
    echo "  Campaign Trigger"
    echo "  File:          ${CAMPAIGN_FILE}"
    echo "  Dry run:       $([[ "$MODE" == "campaign-dry-run" ]] && echo yes || echo no)"
    echo "  Target:        ${TARGET_SERVICE:-all services}"
    echo "============================================"

    for svc in "${SERVICES[@]}"; do
        echo "  -> Triggering campaign on ${svc}"
        trigger_campaign_on_service "$svc" "$payload"
    done
    echo "Done."
}

if [[ "$MODE" == "campaign" || "$MODE" == "campaign-dry-run" ]]; then
    run_campaign_mode
    exit 0
fi

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
