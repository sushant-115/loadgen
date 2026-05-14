#!/usr/bin/env bash
# demo-inject.sh — run anomaly injection commands for InfraSage demos.
#
# Modes:
#   LOCAL  — uses docker-compose exec (default when DEMO_MODE=local or Docker stack is up)
#   REMOTE — uses SSH to gojodb k3s cluster (set DEMO_MODE=remote)
#
# Usage:
#   ./scripts/demo-inject.sh inject <scenario> [duration_sec]
#   ./scripts/demo-inject.sh clear
#   ./scripts/demo-inject.sh status
#   ./scripts/demo-inject.sh scenarios
#   ./scripts/demo-inject.sh chaos <type> [intensity] [duration_sec] [--service <name>]
#   ./scripts/demo-inject.sh campaign --file <path> [--service <name>]
#   ./scripts/demo-inject.sh ciad-discover
#   ./scripts/demo-inject.sh watchdog
#   ./scripts/demo-inject.sh ciad-scores
#   ./scripts/demo-inject.sh pods
#   ./scripts/demo-inject.sh logs [service]
#
# Scenarios (inject):
#   db_contention               DB latency ×5-15, cascades to network_congestion
#   payment_degradation         Payment timeouts/failures; order retries pile up
#   auth_service_degradation    Auth error rate spikes; cascades across services
#   network_saturation          All latency up; cascades to payment+DB
#   memory_pressure             Cache latency, cascades to network_congestion
#   cache_stampede              Hot-key eviction → DB read amplification
#   connection_pool_exhaustion  Bimodal p50/p99 latency pattern
#   retry_amplification         Payment failures → order retry fan-out (3× QPS)
#   clock_skew_auth             ~35% JWT verify failures (auth only)
#   queue_consumer_lag          Queue depth grows → order latency climbs
#
# Chaos types (chaos):
#   cpu / memory / errors / latency / logstorm / db-slow / queue-backlog / pod-crash
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
GOJODB_HOST="${GOJODB_HOST:-3.111.29.131}"
GOJODB_USER="${GOJODB_USER:-ec2-user}"
GOJODB_KEY="${GOJODB_KEY:-${HOME}/.ssh/komal.pem}"
NAMESPACE="${NAMESPACE:-loadgen}"

INFRASAGE_API_URL="${INFRASAGE_API_URL:-https://api.infrasage.dev}"
INFRASAGE_EMAIL="${INFRASAGE_EMAIL:-admin@infrasage.local}"
INFRASAGE_PASSWORD="${INFRASAGE_PASSWORD:-InfraSage@2026!}"

# Auto-detect mode: if DEMO_MODE not set, check if docker-compose stack is up.
_detect_mode() {
  if [[ "${DEMO_MODE:-}" == "remote" ]]; then
    echo "remote"; return
  fi
  if [[ "${DEMO_MODE:-}" == "local" ]]; then
    echo "local"; return
  fi
  # Auto: check if auth-service container is running
  if docker ps --format '{{.Names}}' 2>/dev/null | grep -q "auth-service"; then
    echo "local"
  else
    echo "remote"
  fi
}

MODE=$(_detect_mode)
SSH="ssh -i ${GOJODB_KEY} -o StrictHostKeyChecking=no -o ConnectTimeout=10 ${GOJODB_USER}@${GOJODB_HOST}"

echo "Demo mode: ${MODE}" >&2

# ---------------------------------------------------------------------------
# Injection helpers
# ---------------------------------------------------------------------------

# _exec_on_services <scenario> <duration>
# Calls /anomaly/force on each loadgen service (local: docker-compose exec, remote: kubectl exec)
_inject_scenario() {
  local scenario="$1" duration="$2"
  # Services that expose the anomaly/force endpoint
  local services=("auth-service" "user-service" "order-service" "payment-service" "notification-worker" "gateway")
  local port_map_local=("auth-service:8081" "user-service:8082" "order-service:8083" "payment-service:8084" "notification-worker:8085" "gateway:8080")

  if [[ "${MODE}" == "local" ]]; then
    # Hit each service's HTTP API via docker network (from host via mapped ports or exec)
    # Services port mapping: gateway:8080, auth:8081, user:8082, order:8083, payment:8084, notification:8085
    declare -A svc_ports=(
      [gateway]=8080 [auth-service]=8081 [user-service]=8082
      [order-service]=8083 [payment-service]=8084 [notification-worker]=8085
    )
    for svc in "${!svc_ports[@]}"; do
      local port="${svc_ports[$svc]}"
      curl -s -X POST "http://localhost:${port}/anomaly/force?scenario=${scenario}&duration=${duration}" \
        -o /dev/null && echo "  ✓ ${svc}" || echo "  ✗ ${svc} (skipped)"
    done
  else
    # Remote: SSH → inject-anomaly.sh on gojodb
    ${SSH} "
      export KUBECONFIG=/home/ec2-user/.kube/config
      cd /home/ec2-user/loadgen
      bash scripts/inject-anomaly.sh inject '${scenario}' '${duration}'
    "
  fi
}

_clear_scenario() {
  if [[ "${MODE}" == "local" ]]; then
    declare -A svc_ports=(
      [gateway]=8080 [auth-service]=8081 [user-service]=8082
      [order-service]=8083 [payment-service]=8084 [notification-worker]=8085
    )
    for svc in "${!svc_ports[@]}"; do
      local port="${svc_ports[$svc]}"
      curl -s -X POST "http://localhost:${port}/anomaly/clear" -o /dev/null \
        && echo "  ✓ ${svc} cleared" || echo "  ✗ ${svc} (skipped)"
    done
  else
    ${SSH} "
      export KUBECONFIG=/home/ec2-user/.kube/config
      cd /home/ec2-user/loadgen
      bash scripts/inject-anomaly.sh clear
    "
  fi
}

_status() {
  if [[ "${MODE}" == "local" ]]; then
    declare -A svc_ports=(
      [gateway]=8080 [auth-service]=8081 [user-service]=8082
      [order-service]=8083 [payment-service]=8084 [notification-worker]=8085
    )
    for svc in "${!svc_ports[@]}"; do
      local port="${svc_ports[$svc]}"
      echo -n "  ${svc}: "
      curl -s "http://localhost:${port}/anomaly/status" 2>/dev/null || echo "(unreachable)"
    done
  else
    ${SSH} "
      export KUBECONFIG=/home/ec2-user/.kube/config
      cd /home/ec2-user/loadgen
      bash scripts/inject-anomaly.sh status
    "
  fi
}

_pods() {
  if [[ "${MODE}" == "local" ]]; then
    docker ps --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}" | grep -v "^NAMES" | sort
  else
    ${SSH} "export KUBECONFIG=/home/ec2-user/.kube/config && kubectl -n ${NAMESPACE} get pods"
  fi
}

_logs() {
  local svc="${1:-traffic-generator}"
  if [[ "${MODE}" == "local" ]]; then
    docker logs "${svc}" --tail=50 -f
  else
    ${SSH} "export KUBECONFIG=/home/ec2-user/.kube/config && kubectl -n ${NAMESPACE} logs deployment/${svc} --tail=30 -f"
  fi
}

# ---------------------------------------------------------------------------
# InfraSage API helpers
# ---------------------------------------------------------------------------

# get_bearer_token — logs in and returns a session token for API calls.
get_bearer_token() {
  curl -s -X POST "${INFRASAGE_API_URL}/api/v1/auth/login" \
    -H "Content-Type: application/json" \
    -d "{\"email\":\"${INFRASAGE_EMAIL}\",\"password\":\"${INFRASAGE_PASSWORD}\"}" \
    | jq -r '.token'
}

# ---------------------------------------------------------------------------
# Main dispatch
# ---------------------------------------------------------------------------
cmd="${1:-help}"
shift || true

case "${cmd}" in
  inject)
    scenario="${1:-}"
    duration="${2:-300}"
    if [[ -z "${scenario}" ]]; then
      echo "Usage: $0 inject <scenario> [duration_sec]"
      exit 1
    fi
    echo "Injecting scenario '${scenario}' for ${duration}s  [mode=${MODE}]..."
    _inject_scenario "${scenario}" "${duration}"
    ;;

  clear)
    echo "Clearing all forced scenarios  [mode=${MODE}]..."
    _clear_scenario
    ;;

  status)
    echo "Anomaly status  [mode=${MODE}]:"
    _status
    ;;

  scenarios)
    if [[ "${MODE}" == "local" ]]; then
      echo "Available scenarios:"
      echo "  db_contention, payment_degradation, auth_service_degradation,"
      echo "  network_saturation, memory_pressure, cache_stampede,"
      echo "  connection_pool_exhaustion, retry_amplification,"
      echo "  clock_skew_auth, queue_consumer_lag"
    else
      ${SSH} "
        export KUBECONFIG=/home/ec2-user/.kube/config
        cd /home/ec2-user/loadgen
        bash scripts/inject-anomaly.sh scenarios
      "
    fi
    ;;

  chaos)
    if [[ "${MODE}" == "local" ]]; then
      echo "For local mode, use 'inject' command with a named scenario."
      echo "Direct chaos injection in local mode:"
      echo "  $0 inject <scenario> [duration]"
    else
      args="$*"
      echo "Running chaos: ${args}  [mode=remote]"
      ${SSH} "
        export KUBECONFIG=/home/ec2-user/.kube/config
        cd /home/ec2-user/loadgen
        bash scripts/trigger-chaos-k8s.sh ${args}
      "
    fi
    ;;

  campaign)
    if [[ "${MODE}" == "local" ]]; then
      # Local: run campaign by reading JSON and curl-ing each step
      file=""
      target_service=""
      while [[ $# -gt 0 ]]; do
        case "$1" in
          --file) file="$2"; shift 2 ;;
          --service) target_service="$2"; shift 2 ;;
          *) shift ;;
        esac
      done
      if [[ -z "${file}" ]]; then
        echo "Usage: $0 campaign --file <path.json> [--service <name>]"
        exit 1
      fi
      echo "Running campaign from ${file}  [mode=local]"
      # Use python to parse and execute steps with delays
      python3 - "${file}" "${target_service}" <<'PYEOF'
import sys, json, time, subprocess, urllib.request, urllib.error

campaign_file = sys.argv[1]
target_service = sys.argv[2] if len(sys.argv) > 2 else ""

with open(campaign_file) as f:
    campaign = json.load(f)

steps = campaign.get("steps", [])
start_time = time.time()

svc_ports = {
    "gateway": 8080, "auth-service": 8081, "user-service": 8082,
    "order-service": 8083, "payment-service": 8084, "notification-worker": 8085,
}

for i, step in enumerate(steps):
    delay = step.get("delay_seconds", 0)
    chaos_type = step.get("chaos_type", "error_injection")
    intensity = step.get("intensity", 0.3)
    duration = step.get("duration_seconds", 300)
    services = step.get("services", list(svc_ports.keys()))

    if target_service:
        services = [target_service]

    elapsed = time.time() - start_time
    wait = max(0, delay - elapsed)
    if wait > 0:
        print(f"  Step {i+1}: waiting {wait:.0f}s before '{chaos_type}'...")
        time.sleep(wait)

    print(f"  Step {i+1}: injecting '{chaos_type}' intensity={intensity} duration={duration}s on {services}")
    for svc in services:
        port = svc_ports.get(svc)
        if not port:
            continue
        url = f"http://localhost:{port}/chaos/inject"
        data = json.dumps({"type": chaos_type, "intensity": intensity, "duration_seconds": duration}).encode()
        try:
            req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"}, method="POST")
            urllib.request.urlopen(req, timeout=5)
            print(f"    ✓ {svc}")
        except Exception as e:
            print(f"    ✗ {svc}: {e}")

print("Campaign complete.")
PYEOF
    else
      args="$*"
      echo "Running campaign: ${args}  [mode=remote]"
      ${SSH} "
        export KUBECONFIG=/home/ec2-user/.kube/config
        cd /home/ec2-user/loadgen
        bash scripts/trigger-chaos-k8s.sh campaign ${args}
      "
    fi
    ;;

  ciad-discover)
    # Trigger CIAD fleet discovery on the production InfraSage cluster
    TOKEN=$(get_bearer_token)
    echo "Triggering CIAD discovery..."
    curl -s -X POST \
      -H "Authorization: Bearer ${TOKEN}" \
      "${INFRASAGE_API_URL}/api/v1/ciad/discover" | jq .
    ;;

  watchdog)
    # Show live watchdog summary from InfraSage
    TOKEN=$(get_bearer_token)
    curl -s \
      -H "Authorization: Bearer ${TOKEN}" \
      "${INFRASAGE_API_URL}/api/v1/watchdog/summary" | jq '{
        timestamp,
        summary,
        incidents,
        services: [.services[]? | {service_id, state, severity, z_score, weirdness_score, error_rate_percent, request_latency_ms, likely_cause}]
      }'
    ;;

  ciad-scores)
    TOKEN=$(get_bearer_token)
    curl -s \
      -H "Authorization: Bearer ${TOKEN}" \
      "${INFRASAGE_API_URL}/api/v1/ciad/scores" | jq '{count, scores: [.scores[]? | {service_id, causal_score, rank}]}'
    ;;

  pods)
    _pods
    ;;

  logs)
    svc="${1:-traffic-generator}"
    _logs "${svc}"
    ;;

  *)
    cat <<USAGE
Usage: $0 <command> [args]

  Mode is auto-detected (docker-compose if auth-service container is up, else SSH to gojodb).
  Override: DEMO_MODE=local or DEMO_MODE=remote

Commands:
  inject <scenario> [duration_sec]    Inject a coordinated anomaly (default 300s)
  clear                               Cancel active scenario, return to normal
  status                              Show current forced scenario state per service
  scenarios                           List all available scenario names
  chaos <type> [intensity] [dur] [--service <name>]  Raw chaos (remote only)
  campaign --file <path> [--service]  Run a multi-step chaos campaign from JSON
  ciad-discover                       Trigger CIAD fleet discovery on InfraSage
  watchdog                            Show live watchdog summary from InfraSage
  ciad-scores                         Show CIAD causal scores from InfraSage
  pods                                Show running loadgen pods/containers
  logs [service]                      Tail logs (default: traffic-generator)

Demo sequence:
  1. $0 inject auth_service_degradation 300    → Watchdog demo
  2. $0 clear && $0 inject retry_amplification 600  → CIAD demo
  3. $0 clear && $0 inject queue_consumer_lag 600   → Pillar demo

Environment overrides:
  DEMO_MODE=local|remote
  INFRASAGE_API_URL, INFRASAGE_EMAIL, INFRASAGE_PASSWORD
  GOJODB_HOST, GOJODB_USER, GOJODB_KEY
USAGE
    ;;
esac
