#!/usr/bin/env python3
"""feature-verify — assert shipped features actually DO something.

api-scan.py proves endpoints answer and tenants stay separated. This is
the pass that was missing, and its absence is why the following all
shipped "working":

  * multi-tool chat turns died into an apology for months (assistant
    turns were not replayed as tool_use blocks)
  * a full alert-rules CRUD + console page existed with nothing
    evaluating the rules
  * runbook trust scoring had seven fatal defects; 336 executions
    produced zero rows
  * PII redaction reported itself enabled while applying to a code path
    production does not run
  * service groups showed a hardcoded perfect cohesion score and a
    false algorithm name
  * OTLP rejected the stock collector defaults (protobuf + gzip)

Every one had passing unit tests or a green endpoint. What none had was
a check that looked for EVIDENCE OF EFFECT after the fact: a row that
should exist, a value that should be a delta, a marker that should have
been redacted. That is all this script does.

Checks are arrange -> act -> assert -> clean up, and each one states what
it would have caught.

Usage:
  INFRASAGE_TOKEN=<tenant jwt> INFRASAGE_TENANT=oncall-org \\
  python3 scripts/feature-verify.py [--slow] [--only NAME]

  --slow   include checks that wait on real pipelines (rules ~6min,
           redaction ~2min). Default runs only the fast ones.
  --only   run a single check by name.

Evidence lives in ClickHouse for most checks, reached via kubectl exec
(CH_POD / CH_NS override the defaults). Checks that cannot reach it
report SKIP rather than passing vacuously — a check that cannot see
evidence must never look green.

Exit code = number of FAILED checks (SKIP does not fail the run).
"""
import argparse
import json
import os
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("INFRASAGE_BASE_URL", "https://api.infrasage.dev")
TOKEN = os.environ.get("INFRASAGE_TOKEN", "")
TENANT = os.environ.get("INFRASAGE_TENANT", "oncall-org")
CH_POD = os.environ.get("CH_POD", "infrasage-clickhouse-0")
CH_NS = os.environ.get("CH_NS", "infrasage")

# Cloudflare blocks python-urllib's default UA outright (403), which cost
# an hour the first time. Always present a browser-ish UA.
UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) feature-verify"

PASS, FAIL, SKIP = "PASS", "FAIL", "SKIP"


# ---------------------------------------------------------------- helpers

def api(path, method="GET", body=None, timeout=60):
    """Call the InfraSage API with the tenant token."""
    url = BASE.rstrip("/") + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", f"Bearer {TOKEN}")
    req.add_header("X-Tenant-ID", TENANT)
    req.add_header("User-Agent", UA)
    if data:
        req.add_header("Content-Type", "application/json")
    ctx = ssl.create_default_context()
    with urllib.request.urlopen(req, timeout=timeout, context=ctx) as r:
        raw = r.read().decode()
        try:
            return r.status, json.loads(raw)
        except json.JSONDecodeError:
            return r.status, raw


def ch(query, timeout=60):
    """Run a ClickHouse query via kubectl exec. Returns stripped stdout.

    Raises RuntimeError when the cluster is unreachable so callers can
    report SKIP instead of a vacuous pass.
    """
    cmd = [
        "kubectl", "exec", "-n", CH_NS, CH_POD, "--",
        "sh", "-c",
        'clickhouse-client --user "$CLICKHOUSE_USER" '
        '--password "$CLICKHOUSE_PASSWORD" -d default -q ' + shell_quote(query),
    ]
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    if p.returncode != 0:
        raise RuntimeError((p.stderr or "clickhouse query failed").strip()[:300])
    return p.stdout.strip()


def shell_quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def ch_int(query, default=None):
    out = ch(query)
    if out == "":
        if default is not None:
            return default
        raise RuntimeError("empty result")
    return int(float(out.split()[0]))


# ---------------------------------------------------------------- checks
# Each returns (status, detail). `would_catch` documents the real defect
# the check exists to prevent recurring.

def check_signal_freshness():
    """would_catch: traces and logs rejected with 400 for eight hours
    while metrics kept flowing, because the protobuf bridge emitted enum
    NAMES and only the metrics enum had a tolerant decoder.

    Every aggregate row-count check stayed green throughout: metrics are
    96% of ingest volume, so two of the three signals can go to zero
    without moving the total. Freshness has to be asserted per signal
    type, and the pass condition is every expected type, not any of them.
    """
    expected = {"metric", "trace", "log"}
    rows = ch("SELECT telemetry_type, dateDiff('minute', max(timestamp), now()) "
              "FROM infrasage_raw_firehose "
              "WHERE timestamp > now() - INTERVAL 24 HOUR "
              "GROUP BY telemetry_type")
    if rows == "":
        return FAIL, "no telemetry of any type in the last 24h"

    ages = {}
    for line in rows.splitlines():
        parts = line.split()
        if len(parts) >= 2:
            ages[parts[0]] = int(parts[1])

    missing = sorted(expected - set(ages))
    # 20 minutes tolerates a rollout or a quiet loadgen; a broken signal
    # sits at hours.
    stale = sorted(f"{t} ({ages[t]}m old)" for t in expected & set(ages) if ages[t] > 20)

    if missing:
        return FAIL, (f"no {', '.join(missing)} rows at all in 24h "
                      f"(present: {', '.join(sorted(ages))})")
    if stale:
        return FAIL, f"signal(s) stopped arriving: {', '.join(stale)}"
    return PASS, "all 3 signal types fresh: " + ", ".join(
        f"{t}={ages[t]}m" for t in sorted(expected))


def check_integration_catalog():
    """would_catch: console showing 6 hard-coded providers while the
    platform supported 30."""
    status, body = api("/api/v1/integrations/catalog")
    if status != 200:
        return FAIL, f"HTTP {status}"
    items = body.get("items", [])
    cats = {i["category"] for i in items}
    if len(items) < 25:
        return FAIL, f"only {len(items)} integrations in catalog (expected >=25)"
    if len(cats) < 6:
        return FAIL, f"only {len(cats)} categories (expected >=6)"
    return PASS, f"{len(items)} integrations across {len(cats)} categories"


def check_counter_temporality():
    """would_catch: cumulative OTLP counters stored raw, so every counter
    was an ever-growing staircase whose average was meaningless."""
    q = ("SELECT toUInt64(max(v)) FROM (SELECT window_timestamp, maxMerge(max_value) AS v "
         "FROM infrasage_aggregated_metrics WHERE name = 'http_requests_total' "
         "AND window_timestamp > now() - INTERVAL 30 MINUTE GROUP BY window_timestamp)")
    peak = ch_int(q, default=0)
    if peak == 0:
        return SKIP, "no http_requests_total in the last 30m"
    # A per-interval delta is bounded by traffic; a cumulative counter is
    # unbounded and climbs into the tens of thousands within hours.
    if peak > 10000:
        return FAIL, (f"peak per-window value {peak} looks cumulative, not a "
                      "delta — temporality normalization is not applying")
    return PASS, f"peak per-window value {peak} (delta-shaped)"


def check_histogram_percentiles():
    """would_catch: histogram buckets discarded at ingest, making
    'p99 > 500ms' inexpressible from histogram metrics."""
    q = ("SELECT count() FROM infrasage_aggregated_metrics "
         "WHERE name LIKE '%.p99' AND window_timestamp > now() - INTERVAL 30 MINUTE")
    n = ch_int(q, default=0)
    if n == 0:
        return FAIL, "no .p99 series in the last 30m — bucket percentiles are not being emitted"
    return PASS, f"{n} p99 datapoints in 30m"


def check_instance_rollup():
    """would_catch: per-instance faults averaging away at service grain
    (a disk full on one node of ten reading as 27.8%)."""
    q = ("SELECT count() FROM infrasage_metric_instance_rollup "
         "WHERE instance_id != '' AND window_timestamp > now() - INTERVAL 30 MINUTE")
    n = ch_int(q, default=0)
    if n == 0:
        return FAIL, "instance rollup empty — per-instance detection is blind"
    return PASS, f"{n} instance-keyed windows in 30m"


def check_saturation_armed():
    """would_catch: shipping a detector that silently never evaluates.
    Needs >=60 windows of history before any series is scored."""
    q = ("SELECT max(c) FROM (SELECT count() AS c FROM infrasage_metric_instance_rollup "
         "WHERE window_timestamp > now() - INTERVAL 12 HOUR GROUP BY service_id, name, instance_id)")
    windows = ch_int(q, default=0)
    if windows < 60:
        return FAIL, (f"deepest bounded series has {windows} windows; the detector "
                      "needs >=60 before it evaluates anything")
    return PASS, f"deepest series has {windows} windows (>=60, detector is evaluating)"


def check_trust_scoring():
    """would_catch: 336 runbook executions producing zero trust events,
    because every write referenced columns that do not exist."""
    execs = ch_int("SELECT count() FROM infrasage_runbook_executions", default=0)
    if execs == 0:
        return SKIP, "no runbook executions to judge against"
    events = ch_int("SELECT count() FROM infrasage_runbook_trust_events", default=0)
    summary = ch_int("SELECT count() FROM infrasage_runbook_trust", default=0)
    if events == 0:
        return FAIL, f"{execs} executions but 0 trust events — the write path is broken"
    if summary == 0:
        return FAIL, f"{events} trust events but 0 summary rows — recompute is broken"
    return PASS, f"{execs} executions, {events} events, {summary} summary rows"


def check_group_cohesion_is_measured():
    """would_catch: cohesion_score hardcoded to a perfect 1.0 and
    discovered_via claiming 'louvain' while running connected components."""
    q = ("SELECT count() FROM infrasage_service_groups "
         "WHERE discovered_via = 'louvain' AND updated_at > now() - INTERVAL 2 DAY")
    stale = ch_int(q, default=0)
    if stale > 0:
        return FAIL, f"{stale} groups still labelled 'louvain' (algorithm is connected-components)"
    total = ch_int("SELECT count() FROM infrasage_service_groups", default=0)
    if total == 0:
        return SKIP, "no service groups discovered yet"
    perfect = ch_int("SELECT countIf(cohesion_score = 1) FROM infrasage_service_groups", default=0)
    if total > 2 and perfect == total:
        return FAIL, (f"all {total} groups report cohesion exactly 1.0 — "
                      "the score is almost certainly hardcoded again")
    return PASS, f"{total} groups, {perfect} at cohesion 1.0"


def check_copilot_stream():
    """would_catch: multi-tool chat turns dying into an apology because
    assistant turns were not replayed as tool_use blocks."""
    status, sess = api("/api/v1/chat/sessions", "POST",
                       {"title": "feature-verify"})
    if status != 200:
        return FAIL, f"session create HTTP {status}"
    sid = sess["id"]
    url = f"{BASE.rstrip('/')}/api/v1/chat/sessions/{sid}/messages/stream"
    req = urllib.request.Request(
        url, data=json.dumps({"text": "What alerts are firing right now?"}).encode(),
        method="POST")
    req.add_header("Authorization", f"Bearer {TOKEN}")
    req.add_header("X-Tenant-ID", TENANT)
    req.add_header("Content-Type", "application/json")
    req.add_header("User-Agent", UA)
    seen = set()
    tool_ok = False
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            for raw in r:
                line = raw.decode(errors="replace").strip()
                if not line.startswith("data:"):
                    continue
                try:
                    ev = json.loads(line[5:])
                except json.JSONDecodeError:
                    continue
                seen.add(ev.get("type"))
                if ev.get("type") == "tool_end" and not ev.get("tool_error"):
                    tool_ok = True
                if ev.get("type") in ("done", "error"):
                    break
    except Exception as e:  # noqa: BLE001 - report, don't crash the suite
        return FAIL, f"stream failed: {e}"
    finally:
        try:
            api(f"/api/v1/chat/sessions/{sid}", "DELETE")
        except Exception:  # noqa: BLE001
            pass
    if "error" in seen:
        return FAIL, "stream produced an error event"
    if "done" not in seen:
        return FAIL, f"no terminal done event (saw {sorted(seen)})"
    if not tool_ok:
        return FAIL, ("turn completed without a successful tool call — the "
                      "tool loop is the thing most likely to be silently broken")
    return PASS, f"streamed {sorted(seen)} with a successful tool call"


def check_pii_redaction():
    """SLOW. would_catch: a privacy control reporting itself enabled while
    applying only to a code path production does not run."""
    marker_mail = "verify-probe@example.com"
    marker_key = "AKIAIOSFODNN7EXAMPLE"
    probe_id = "feature-verify-redaction"
    svc = f"{TENANT}/api-gateway"
    ch(f"""INSERT INTO infrasage_raw_firehose
        (timestamp, telemetry_type, service_id, name, attributes, value,
         log_body, trace_id, span_id, tenant_id, environment, instance_id)
        VALUES (now() - INTERVAL 2 MINUTE, 'log', '{svc}', 'otlp.log',
        {{'level':'error'}}, 1,
        'ERROR checkout failed for {marker_mail} key {marker_key}',
        '', '', '{TENANT}', 'prod', '{probe_id}')""")
    try:
        ts = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 120))
        api("/api/v1/rca/analyze", "POST",
            {"service_id": "api-gateway", "timestamp": ts}, timeout=120)
        deadline = time.time() + 240
        while time.time() < deadline:
            time.sleep(20)
            leaked = ch_int(
                "SELECT countIf(position(tool_result, '%s') > 0) "
                "FROM infrasage_rca_agent_steps WHERE ts > now() - INTERVAL 15 MINUTE"
                % marker_mail, default=0)
            tagged = ch_int(
                "SELECT countIf(position(tool_result, '[EMAIL]') > 0) "
                "FROM infrasage_rca_agent_steps WHERE ts > now() - INTERVAL 15 MINUTE",
                default=0)
            if leaked:
                return FAIL, f"raw PII reached the agent trail ({leaked} rows) — redaction is not applying"
            if tagged:
                return PASS, f"PII replaced with typed placeholders ({tagged} rows), no leaks"
        return SKIP, "RCA did not surface the probe log within the window"
    finally:
        try:
            ch(f"ALTER TABLE infrasage_raw_firehose DELETE WHERE instance_id = '{probe_id}'")
        except Exception:  # noqa: BLE001
            pass


def check_alert_rules_engine():
    """SLOW. would_catch: a rules CRUD and console page whose rules
    nothing ever evaluated."""
    status, created = api("/api/v1/alert-rules", "POST", {
        "name": "feature-verify probe", "type": "threshold",
        "service_id": "api-gateway", "metric": "http_requests_total",
        "comparison": "above", "threshold": 0, "for_duration": "5m",
        "severity": "low", "enabled": True,
    })
    if status not in (200, 201):
        return FAIL, f"rule create HTTP {status}"
    rid = created["id"]
    try:
        deadline = time.time() + 420
        while time.time() < deadline:
            time.sleep(30)
            n = ch_int(
                f"SELECT count() FROM infrasage_rule_fires WHERE rule_id = '{rid}'",
                default=0)
            if n:
                return PASS, f"rule fired {n} time(s) against live data"
        return FAIL, ("rule never fired in 7 minutes against a threshold of 0 — "
                      "the engine is not evaluating rules")
    finally:
        try:
            api(f"/api/v1/alert-rules/{rid}", "DELETE")
            ch(f"ALTER TABLE infrasage_rule_fires DELETE WHERE rule_id = '{rid}'")
        except Exception:  # noqa: BLE001
            pass


FAST = [
    ("signal_freshness", check_signal_freshness),
    ("integration_catalog", check_integration_catalog),
    ("counter_temporality", check_counter_temporality),
    ("histogram_percentiles", check_histogram_percentiles),
    ("instance_rollup", check_instance_rollup),
    ("saturation_armed", check_saturation_armed),
    ("trust_scoring", check_trust_scoring),
    ("group_cohesion", check_group_cohesion_is_measured),
    ("copilot_stream", check_copilot_stream),
]
SLOW = [
    ("pii_redaction", check_pii_redaction),
    ("alert_rules_engine", check_alert_rules_engine),
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--slow", action="store_true",
                    help="include checks that wait on real pipelines")
    ap.add_argument("--only", help="run a single check by name")
    args = ap.parse_args()

    if not TOKEN:
        print("INFRASAGE_TOKEN is required", file=sys.stderr)
        return 2

    checks = FAST + (SLOW if args.slow else [])
    if args.only:
        checks = [c for c in checks if c[0] == args.only] or \
                 [c for c in FAST + SLOW if c[0] == args.only]
        if not checks:
            print(f"no such check: {args.only}", file=sys.stderr)
            return 2

    failures = 0
    results = []
    for name, fn in checks:
        started = time.time()
        try:
            status, detail = fn()
        except RuntimeError as e:
            status, detail = SKIP, f"evidence unreachable: {e}"
        except Exception as e:  # noqa: BLE001
            status, detail = FAIL, f"check raised: {type(e).__name__}: {e}"
        took = time.time() - started
        results.append((status, name, detail, took))
        if status == FAIL:
            failures += 1
        print(f"{status:4}  {name:24} {detail}  ({took:.0f}s)", flush=True)

    print("\n" + "-" * 72)
    counts = {s: sum(1 for r in results if r[0] == s) for s in (PASS, FAIL, SKIP)}
    print(f"{counts[PASS]} passed, {counts[FAIL]} failed, {counts[SKIP]} skipped")
    if counts[SKIP]:
        print("NOTE: skipped checks saw no evidence either way — they are not passes.")
    return failures


if __name__ == "__main__":
    sys.exit(main())
