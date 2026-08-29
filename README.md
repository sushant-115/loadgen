# loadgen — Production Simulator + InfraSage Verdict Harness

A microservice fleet that behaves like production (diurnal traffic, coherent
user journeys, OTel logs/metrics/traces), a chaos engine that fails like
production (ramps, slow leaks, flapping, partial scopes, low-grade burns —
not just step functions), and a **verdict harness** that fires graded
scenarios and interrogates InfraSage's own APIs to score the outcome:
detected or not, how fast, right origin or wrong, change attributed or
missed, remediation real or cosmetic.

```
traffic (journeys × loadshape) → services → OTel collector → InfraSage
                                    ↑                            ↓
              verdict harness ── chaos/ops endpoints ── alerts/RCA/runbook APIs
```

## Services

| Service | Port | Description |
|---------|------|-------------|
| gateway | 8080 | API gateway, proxies to backend services |
| auth | 8081 | Authentication (login, token validation) |
| user | 8082 | User CRUD |
| order | 8083 | Order management, calls payment-service |
| payment | 8084 | Payment processing |
| notification | 8085 | Async notification worker (NATS) |
| mock-shopify | — | E-commerce API with injectable KPI incidents |
| traffic | — | Journey-based traffic generator |

## Traffic: journeys on a load curve

`TRAFFIC_MODE=journeys` (default) runs coherent user **sessions** — login →
browse → order → pay, with lognormal think-times and realistic abandonment —
launched at a rate that follows a **loadshape curve**: two-humped diurnal
rhythm, lunch dip, weekend factor, Monday bump, and mean-reverting organic
noise. During injected incidents traffic *drops* (users bounce; they don't
5× at the worst moment). Baselines finally learn from honest data.

Env knobs: `REQUESTS_PER_SECOND` (base), `LOADSHAPE_DIURNAL_AMPLITUDE`,
`LOADSHAPE_WEEKEND_FACTOR`, `LOADSHAPE_LUNCH_DIP`, `LOADSHAPE_NOISE_SIGMA`,
`LOADSHAPE_TIME_COMPRESSION` (24 = a day per hour, for local smoke tests),
`TRAFFIC_MODE=legacy` restores the old weighted-action loop.

## Chaos v2: shapes, scope, sticky

Every `/chaos/*` endpoint accepts shape fields on top of the legacy
`{intensity, duration_seconds}`:

```jsonc
POST /chaos/db-slow
{
  "intensity": 0.7,
  "duration_seconds": 1200,
  "onset": "ramp",          // step | ramp | slowleak | flap
  "ramp_seconds": 300,
  "flap_period_seconds": 90, // for onset=flap
  "scope_percent": 0.03,     // fraction of traffic affected
  "sticky": true             // ignores duration — only /ops/* or kill-switch clears it
}
```

Fault types: the classics (`errors`, `latency`, `cpu`, `memory`, `logstorm`,
`db-slow`, `queue-backlog`, `pod-crash`) plus three subtle ones —
`novel-log` (a never-seen log template at low volume — tests shape
detection, not volume), `error-budget` (2% quiet failure burn), and
`latency-tail` (p99 moves, the average barely does). Campaigns, kill-switch,
and `/chaos/status` work as before.

## /ops: remediation that remediates

Each service exposes `/ops/restart`, `/ops/reset-pool`, `/ops/clear-backlog`,
`/ops/scale {"replicas":N}`, `/ops/status`. Each remediation clears only the
fault kinds it plausibly fixes (reset-pool clears `db_slow`, nothing else),
and `scale` divides capacity-sensitive fault intensity. Point an InfraSage
runbook's `legacy:http` step at these and the propose→approve→execute→
recover loop becomes **testable**: a sticky fault recovers if and only if
the right runbook ran. Ready-to-import runbook specs live in
[`infrasage-runbooks/`](infrasage-runbooks/) (replace `LOADGEN_HOST`).

## The verdict harness

Scenarios in [`scenarios/`](scenarios/) carry their ground truth; `verdict`
runs them and grades InfraSage:

```bash
go build -o verdict ./cmd/verdict

export INFRASAGE_BASE_URL=https://api.your-infrasage
export INFRASAGE_TOKEN=…            # ordinary operator token — the harness plays the human
export LOADGEN_HOST=http://localhost # or the loadgen host

./verdict list
./verdict run --id s02-payment-dbslow-ramp-change
./verdict suite                      # everything, paced, exits non-zero on failure
./verdict suite --tags detection,subtle --skip-remediation
./verdict reset                      # kill-switch every fault everywhere
```

A run walks: preflight (refuses to grade a degraded engine) → change events
(deploys before faults, so change-attribution is graded) → injection →
detection watch (latency measured) → RCA (triggered explicitly — the
no-auto-RCA posture is exercised, not bypassed; rule-based-fallback runs are
marked `degraded`, never counted as accuracy failures) → remediation
(manual-trigger + approve via the real APIs, recovery verified on the
loadgen side) → business KPI check → kill-switch + cooldown (waits for
InfraSage's RCA queue to drain — pacing is a first-class feature). Output:
`reports/verdict-<ts>.md` + `.json` scorecards.

The library covers the matrix: a sanity step fault (s01), the flagship
full-loop deploy→ramp→RCA→remediation (s02), subtle detection (s03 error
burn, s04 tail latency, s05 slow leak, s07 novel log), alert coherence
under flapping (s08), the **false-positive control** — an innocent deploy
that must stay quiet (s09), sticky-backlog remediation (s06), and a
business-KPI slump with healthy infrastructure (s10).

## BYO-stack parity testbed

The compose/k8s stack already runs Prometheus, Loki, and Jaeger.
`scripts/federated-testbed.sh` registers them as InfraSage **federated
sources** (aggregate-pull detection + question-time evidence, no raw
custody) — then run the same detection scenarios against the federated
tenant and diff the scorecards against ingest mode.

## Deploy

Local: `docker compose up` (collector forwards to Jaeger/Loki/Prometheus +
InfraSage via `otlphttp/infrasage`). k3s host: build binaries, import the
image, `kubectl apply -f k8s-deployment.yaml -f k8s-services.yaml` — or
`./scripts/deploy-k8s-infrasage.sh` with `INFRASAGE_API_KEY` set (creates
the `infrasage-credentials` secret; never commit keys). Chaos helpers:
`./scripts/trigger-chaos.sh`, `./scripts/trigger-chaos-k8s.sh`.

All loadgen telemetry is stamped `infrasage_synthetic=true`.
