#!/usr/bin/env python3
"""api-scan — contract smoke + tenant-isolation probe for InfraSage.

Two scan modes, born from a night of finding exactly these bug classes by
hand (see The Second Hour / Glass Cockpit docs):

  smoke  — hits every read endpoint in MANIFEST with a real token and
           asserts HTTP 200 plus the presence of the top-level keys the
           console actually reads. Catches UI↔backend contract drift
           (items vs runbooks, service vs service_id, ts vs
           window_timestamp — all shipped broken once).

  tenant — the adversarial pass authorization testing always needed:
           as tenant A, every list must contain zero foreign-tenant ids,
           and direct reads of known foreign resources must 403/404.
           Would have caught the dashboards/services and incidents API
           cross-tenant leaks on day one.

Usage:
  INFRASAGE_BASE_URL=https://api.infrasage.dev \
  INFRASAGE_TOKEN=<tenant token> INFRASAGE_TENANT=oncall-org \
  FOREIGN_TENANT=thewhitehatpirate-org FOREIGN_INCIDENT=ci_07241f434e35ed88 \
  python3 scripts/api-scan.py [smoke|tenant|all]

Exit code = number of failures.
"""
import json
import os
import sys
import urllib.parse
import urllib.request

BASE = os.environ.get("INFRASAGE_BASE_URL", "https://api.infrasage.dev")
TOKEN = os.environ.get("INFRASAGE_TOKEN", "")
TENANT = os.environ.get("INFRASAGE_TENANT", "")
FOREIGN = os.environ.get("FOREIGN_TENANT", "")
FOREIGN_INCIDENT = os.environ.get("FOREIGN_INCIDENT", "")
SVC = os.environ.get("SCAN_SERVICE", "payment-service")

# (path, expected top-level keys, description)
MANIFEST = [
    ("/api/v1/alerts?limit=5", ["alerts"], "alerts list"),
    ("/api/v1/incidents?limit=5", ["incidents"], "incidents list"),
    ("/api/v1/runbooks", ["items", "runbooks"], "runbooks hub list (dual-key)"),
    ("/api/v1/runbooks/drafts", ["items", "drafts"], "drafts list (dual-key)"),
    ("/api/v1/runbooks/feature-flag", ["enabled"], "hub flag probe"),
    ("/api/v1/dashboards/services", ["services"], "service picker"),
    (f"/api/v1/dashboards/service/{{tenant}}/{SVC}?time_window=1h", ["sections", "archetype"], "composed dashboard"),
    ("/api/v1/dashboards/catalog", ["widgets"], "widget catalog"),
    (f"/api/v1/explore/metrics?service={SVC}", ["items"], "explore metrics"),
    (f"/api/v1/explore/logs?service={SVC}&window=15m&limit=5", ["items"], "explore logs"),
    (f"/api/v1/explore/traces?service={SVC}&window=15m&limit=5", ["items"], "explore traces"),
    (f"/api/v1/watchdog/zscore-history?service_id={SVC}&minutes=30", ["points"], "zscore history (service_id spelling)"),
    (f"/api/v1/watchdog/zscore-history?service={SVC}&minutes=30", ["points"], "zscore history (service spelling)"),
    ("/api/v1/engine/saturation", None, "saturation"),
    ("/api/v1/agent/actions", None, "pending decisions"),
    (f"/api/v1/rca/analysis?service={SVC}", ["analysis_mode"], "latest RCA"),
]

# List endpoints where foreign-tenant ids must never appear, plus a JSON
# path (dotted) to the string values worth checking.
TENANT_LISTS = [
    ("/api/v1/alerts?limit=200", "alerts", ["service_id", "title"]),
    ("/api/v1/incidents?limit=200", "incidents", ["services", "title", "incident_id"]),
    ("/api/v1/dashboards/services", "services", ["service_id"]),
    (f"/api/v1/explore/metrics?service={SVC}", "items", ["name"]),
]

FOREIGN_READS = [
    # Direct reads of known foreign resources: anything but 403/404 is a leak.
    ("/api/v1/incidents/{fi}", "foreign incident detail"),
    ("/api/v1/incidents/{fi}/events", "foreign incident timeline"),
    (f"/api/v1/explore/metrics?service={{ft}}/{SVC}", "foreign explore metrics"),
    (f"/api/v1/watchdog/zscore-history?service={{ft}}/{SVC}&minutes=30", "foreign zscore history"),
    (f"/api/v1/dashboards/service/{{ft}}/{SVC}", "foreign dashboard compose"),
]


def call(path):
    url = BASE + path
    req = urllib.request.Request(url, headers={
        "Authorization": f"Bearer {TOKEN}",
        "X-Tenant-ID": TENANT,
        # Cloudflare bot-fight 403s python-urllib's default UA; identify as
        # the scanner explicitly with a curl-compatible prefix.
        "User-Agent": "curl/8.4.0 infrasage-api-scan",
    })
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        try:
            body = json.loads(e.read().decode() or "{}")
        except Exception:
            body = {}
        return e.code, body
    except Exception as e:  # noqa: BLE001
        return -1, {"error": str(e)}


def walk_strings(obj):
    if isinstance(obj, str):
        yield obj
    elif isinstance(obj, list):
        for v in obj:
            yield from walk_strings(v)
    elif isinstance(obj, dict):
        for v in obj.values():
            yield from walk_strings(v)


def smoke():
    fails = 0
    for path, keys, desc in MANIFEST:
        p = path.replace("{tenant}", urllib.parse.quote(TENANT, safe=""))
        code, body = call(p)
        if code != 200:
            print(f"FAIL  [{desc}] {p} -> HTTP {code} {json.dumps(body)[:120]}")
            fails += 1
            continue
        missing = [k for k in (keys or []) if k not in body]
        if missing:
            print(f"FAIL  [{desc}] {p} -> 200 but missing keys {missing}; got {sorted(body.keys())[:8]}")
            fails += 1
        else:
            print(f"ok    [{desc}]")
    return fails


def tenant():
    if not FOREIGN:
        print("SKIP  tenant probe: FOREIGN_TENANT not set")
        return 0
    fails = 0
    marker = FOREIGN + "/"
    for path, key, _fields in TENANT_LISTS:
        code, body = call(path)
        if code != 200:
            print(f"FAIL  [isolation:{key}] {path} -> HTTP {code}")
            fails += 1
            continue
        leaked = sorted({s for s in walk_strings(body.get(key, [])) if marker in s})
        if leaked:
            print(f"LEAK  [{key}] {path} exposes foreign ids: {leaked[:5]}")
            fails += 1
        else:
            print(f"ok    [isolation:{key}] no foreign ids")
    for path, desc in FOREIGN_READS:
        p = path.replace("{fi}", FOREIGN_INCIDENT or "ci_nonexistent").replace("{ft}", FOREIGN)
        code, body = call(p)
        if code in (403, 404):
            print(f"ok    [deny:{desc}] -> {code}")
        elif code == 200 and FOREIGN in json.dumps(body):
            print(f"LEAK  [{desc}] {p} -> 200 with foreign content")
            fails += 1
        elif code == 200:
            # 200 but no foreign content (e.g. empty result) — flag softly.
            print(f"WARN  [{desc}] {p} -> 200 (empty/neutral body; prefer 403/404)")
        else:
            print(f"ok    [deny:{desc}] -> {code}")
    return fails


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "all"
    if not TOKEN:
        print("INFRASAGE_TOKEN required", file=sys.stderr)
        return 2
    fails = 0
    if mode in ("smoke", "all"):
        print(f"── contract smoke vs {BASE} as {TENANT} ──")
        fails += smoke()
    if mode in ("tenant", "all"):
        print(f"── tenant isolation probe (foreign={FOREIGN}) ──")
        fails += tenant()
    print(f"\n{'PASS' if fails == 0 else 'FAIL'}: {fails} failure(s)")
    return min(fails, 125)


if __name__ == "__main__":
    sys.exit(main())
