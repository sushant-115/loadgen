# loadgen — Microservice Simulator

A lightweight microservice simulator that generates production-like OTEL telemetry (traces, metrics, logs). Built for testing observability platforms like InfraSage.

## Services

| Service | Port | Description |
|---------|------|-------------|
| gateway | 8080 | API gateway, proxies to backend services |
| auth | 8081 | Authentication (login, token validation) |
| user | 8082 | User CRUD |
| order | 8083 | Order management, calls payment-service |
| payment | 8084 | Payment processing |
| notification | 8085 | Async notification worker (NATS) |
| traffic | — | Traffic generator, sends requests to gateway |

## Chaos Injection

Each service exposes chaos endpoints to simulate failures:

```
POST /chaos/errors         {"intensity": 0.9, "duration_seconds": 300}
POST /chaos/latency        {"intensity": 0.8, "duration_seconds": 300}
POST /chaos/cpu
POST /chaos/memory
POST /chaos/logstorm
POST /chaos/db-slow
POST /chaos/queue-backlog
POST /chaos/pod-crash
POST /chaos/campaign       {"campaign_id":"...","steps":[...],"dry_run":false}
POST /chaos/kill-switch
DELETE /chaos/<type>        # disable
GET /chaos/status
```

Notes:
- `intensity` accepts either ratio (`0.25`) or percentage (`25`).
- Guardrails cap anomaly duration to 15 minutes per step.
- `/chaos/status` now includes active types and campaign metadata for correlation.

### Campaign Example

Use [configs/chaos-campaign.example.json](configs/chaos-campaign.example.json):

```bash
# local/docker
./scripts/trigger-chaos.sh campaign --file configs/chaos-campaign.example.json

# validate only
./scripts/trigger-chaos.sh campaign --file configs/chaos-campaign.example.json --dry-run

# scheduled execution (delay in seconds)
./scripts/trigger-chaos.sh campaign --file configs/chaos-campaign.example.json --schedule-after 120

# adaptive execution (wait until check command exits 0)
./scripts/trigger-chaos.sh campaign --file configs/chaos-campaign.example.json \
  --adaptive-check "curl -sf http://localhost:8080/health >/dev/null" --adaptive-poll 10
```

Kubernetes:

```bash
./scripts/trigger-chaos-k8s.sh campaign --file configs/chaos-campaign.example.json
./scripts/trigger-chaos-k8s.sh latency high 60 --service payment-service
```

### Deploy To K8s With InfraSage Ingest

Use the helper script to apply manifests, upsert secret values, and roll collector:

```bash
export INFRASAGE_API_KEY=your_key_here
./scripts/deploy-k8s-infrasage.sh
```

Notes:
- Do not commit API keys to this repository.
- The script creates/updates the Kubernetes secret `infrasage-credentials` at deploy time using `INFRASAGE_API_KEY`.

Optional override:

```bash
export NAMESPACE=loadgen
./scripts/deploy-k8s-infrasage.sh
```

## Deploy to k3s

```bash
# 1. Build all binaries
export PATH=$PATH:/usr/local/go/bin
mkdir -p /tmp/loadgen-build
for svc in gateway auth user order payment notification traffic; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/loadgen-build/$svc ./cmd/$svc/
done

# 2. Create Docker image
cat > /tmp/loadgen-build/Dockerfile <<'EOF'
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata curl
WORKDIR /app
COPY gateway auth user order payment notification traffic ./
EOF

cd /tmp/loadgen-build && docker build -t loadgen:latest .

# 3. Import into k3s
docker save loadgen:latest | sudo k3s ctr images import -

# 4. Deploy infrastructure (postgres, redis, nats, otel-collector, etc.)
sudo kubectl apply -f k8s-deployment.yaml

# 5. Deploy services
sudo kubectl apply -f k8s-services.yaml
```

## OTEL Telemetry

Services emit traces and metrics via gRPC to port 4317. The included otel-collector config forwards to:
- **Jaeger** (traces)
- **Loki** (logs)
- **Prometheus** (metrics)
- **InfraSage** (all signals via `otlphttp/infrasage` exporter)

Configure the InfraSage endpoint in `k8s-deployment.yaml` under the otel-collector ConfigMap.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | OTEL collector gRPC endpoint |
| `OTEL_SERVICE_NAME` | (per service) | Service name in telemetry |
| `POSTGRES_DSN` | — | PostgreSQL connection string |
| `REDIS_URL` | — | Redis connection string |
| `NATS_URL` | — | NATS connection string |
| `TARGET_URL` | `http://gateway:8080` | Traffic generator target |
| `REQUESTS_PER_SECOND` | `10` | Traffic generator RPS |
| `BURST_INTERVAL_SECONDS` | `300` | Seconds between 5x burst periods |
| `TRAFFIC_SCENARIO_FILE` | (empty) | Optional path to JSON scenario file for weighted traffic actions |

### Traffic Scenario File

The traffic generator supports a scenario file to model weighted, dependency-aware actions.

Use the default example at `configs/traffic-scenario.default.json`:

```bash
export TRAFFIC_SCENARIO_FILE=configs/traffic-scenario.default.json
```

Scenario schema:

```json
{
  "name": "default-production-like",
  "description": "Weighted API traffic with dependency-aware actions",
  "actions": [
    { "name": "users_list", "weight": 0.30 },
    { "name": "users_get", "weight": 0.10 },
    { "name": "auth_login", "weight": 0.15 },
    { "name": "auth_verify", "weight": 0.05 },
    { "name": "orders_create", "weight": 0.20 },
    { "name": "orders_list", "weight": 0.10 },
    { "name": "orders_get", "weight": 0.05 },
    { "name": "users_create", "weight": 0.05 }
  ]
}
```
