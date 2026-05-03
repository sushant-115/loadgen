#!/usr/bin/env bash
# inject-anomaly.sh — inject or clear a coordinated anomaly across all loadgen
# services running in Kubernetes.
#
# The script calls POST /anomaly/force or POST /anomaly/clear on every service
# pod simultaneously via kubectl exec, so all pods see the scenario change at
# the same instant, producing perfectly correlated telemetry spikes.
#
# Usage:
#   ./inject-anomaly.sh inject <scenario> [duration_seconds]
#   ./inject-anomaly.sh clear
#   ./inject-anomaly.sh status
#   ./inject-anomaly.sh scenarios
#
# Scenarios:
#   db_contention           — DB latency ×5-15, error rates climb, cascades to
#                             network_congestion
#   payment_degradation     — payment timeouts 1%→25%, failures 5%→40%
#   auth_service_degradation — auth error rate 1%→20%, wrong-password 5%→35%
#   network_saturation      — all latency increases; cascades to payment+DB
#   memory_pressure         — cache latency widens, gradual latency increase;
#                             cascades to network_congestion
#
# Example:
#   ./inject-anomaly.sh inject network_saturation 600
#   ./inject-anomaly.sh status
#   ./inject-anomaly.sh clear
set -euo pipefail

NAMESPACE="${LOADGEN_NAMESPACE:-loadgen}"
DEFAULT_DURATION="${ANOMALY_DURATION:-300}"
KUBECTL="${KUBECTL:-sudo kubectl}"

# Map of service name → pod label selector
declare -A SERVICES=(
  [auth-service]="app=auth-service"
  [gateway]="app=gateway"
  [user-service]="app=user-service"
  [order-service]="app=order-service"
  [payment-service]="app=payment-service"
  [notification-worker]="app=notification-worker"
)

# Per-service ports (each service binds to its own port)
declare -A SVC_PORT=(
  [gateway]=8080
  [auth-service]=8081
  [user-service]=8082
  [order-service]=8083
  [payment-service]=8084
  [notification-worker]=8085
)

usage() {
  echo "Usage: $0 inject <scenario> [duration_sec]"
  echo "       $0 clear"
  echo "       $0 status"
  echo "       $0 scenarios"
  exit 1
}

# get_pod SVC_NAME — return the first running pod name for a service
get_pod() {
  local svc="$1"
  $KUBECTL get pods -n "$NAMESPACE" \
    -l "${SERVICES[$svc]}" \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# exec_curl SVC POD URL METHOD — run curl inside the pod on the service's port
exec_curl() {
  local svc="$1"
  local pod="$2"
  local url="$3"
  local method="${4:-GET}"
  local port="${SVC_PORT[$svc]:-8080}"
  $KUBECTL exec -n "$NAMESPACE" "$pod" -- \
    /usr/bin/curl -sf -X "$method" "http://localhost:${port}${url}" 2>/dev/null \
    || echo '{"error":"curl failed"}'
}

# run_on_all ACTION_FN — run a function on all service pods in parallel
run_on_all() {
  local action_fn="$1"
  shift
  local pids=()
  local svc

  echo ""
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  printf "%-26s %-12s %s\n" "SERVICE" "STATE" "DETAIL"
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

  for svc in "${!SERVICES[@]}"; do
    {
      local pod
      pod=$(get_pod "$svc")
      if [[ -z "$pod" ]]; then
        printf "%-26s %-12s %s\n" "$svc" "ERROR" "no running pod found"
        exit 0
      fi
      "$action_fn" "$svc" "$pod" "$@"
    } &
    pids+=($!)
  done

  for pid in "${pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
}

do_inject() {
  local svc="$1"
  local pod="$2"
  local scenario="$3"
  local duration="$4"
  local resp
  resp=$(exec_curl "$svc" "$pod" "/anomaly/force?scenario=${scenario}&duration=${duration}" POST)
  local state health faults
  state=$(echo "$resp"  | grep -o '"state":"[^"]*"'   | cut -d'"' -f4 2>/dev/null || echo "?")
  health=$(echo "$resp" | grep -o '"health_score":[0-9.]*' | cut -d: -f2  2>/dev/null || echo "?")
  faults=$(echo "$resp" | grep -o '"active_faults":"[^"]*"' | cut -d'"' -f4 2>/dev/null || echo "?")
  printf "%-26s %-12s health=%-6s faults=%s\n" "$svc" "$state" "$health" "$faults"
}

do_clear() {
  local svc="$1"
  local pod="$2"
  local resp
  resp=$(exec_curl "$svc" "$pod" "/anomaly/clear" POST)
  local state
  state=$(echo "$resp" | grep -o '"state":"[^"]*"' | cut -d'"' -f4 2>/dev/null || echo "?")
  printf "%-26s %-12s %s\n" "$svc" "$state" "override cleared"
}

do_status() {
  local svc="$1"
  local pod="$2"
  local resp
  resp=$(exec_curl "$svc" "$pod" "/anomaly/status" GET)
  local active state health remaining
  active=$(echo "$resp"    | grep -o '"active":[^,}]*'      | cut -d: -f2 2>/dev/null | tr -d ' ')
  state=$(echo "$resp"     | grep -o '"state":"[^"]*"'       | cut -d'"' -f4 2>/dev/null || echo "?")
  health=$(echo "$resp"    | grep -o '"health_score":[0-9.]*'| cut -d: -f2  2>/dev/null || echo "?")
  remaining=$(echo "$resp" | grep -o '"remaining_sec":[0-9]*'| cut -d: -f2 2>/dev/null || echo "")
  local detail="health=${health}"
  if [[ "$active" == "true" && -n "$remaining" ]]; then
    scenario=$(echo "$resp" | grep -o '"scenario":"[^"]*"' | cut -d'"' -f4 2>/dev/null)
    detail="health=${health} scenario=${scenario} remaining=${remaining}s"
  fi
  printf "%-26s %-12s %s\n" "$svc" "$state" "$detail"
}

# ---------- main ----------

CMD="${1:-}"
[[ -z "$CMD" ]] && usage

case "$CMD" in
  inject)
    SCENARIO="${2:-}"
    DURATION="${3:-$DEFAULT_DURATION}"
    [[ -z "$SCENARIO" ]] && { echo "Error: scenario required"; usage; }
    echo ">>> Injecting anomaly: scenario='${SCENARIO}' duration=${DURATION}s across all pods..."
    run_on_all do_inject "$SCENARIO" "$DURATION"
    echo ""
    echo "Anomaly active. Watch signals:"
    echo "  Traces  → Jaeger:     kubectl port-forward svc/jaeger -n $NAMESPACE 16686:16686"
    echo "  Metrics → Prometheus: kubectl port-forward svc/prometheus -n $NAMESPACE 9090:9090"
    echo "  Logs    → Loki/Grafana: kubectl port-forward svc/grafana -n $NAMESPACE 3000:3000"
    echo ""
    echo "Clear early: $0 clear"
    ;;

  clear)
    echo ">>> Clearing all forced anomalies..."
    run_on_all do_clear
    echo ""
    echo "All pods returned to epoch-based health computation."
    ;;

  status)
    echo ">>> Current anomaly status across all pods..."
    run_on_all do_status
    ;;

  scenarios)
    # Pick any pod — scenario list is static
    FIRST_SVC="gateway"
    POD=$(get_pod "$FIRST_SVC")
    if [[ -z "$POD" ]]; then
      echo "Error: no running pod found for $FIRST_SVC"
      exit 1
    fi
    echo ">>> Available scenarios:"
    exec_curl "$FIRST_SVC" "$POD" "/anomaly/scenarios" GET | \
      grep -o '"[a-z_]*"' | tr -d '"' | grep -v scenarios | sed 's/^/  - /'
    ;;

  *)
    usage
    ;;
esac
