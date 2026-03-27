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
DELETE /chaos/<type>        # disable
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
