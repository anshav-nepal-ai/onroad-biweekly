#!/usr/bin/env python3
"""
Refresh rerun snippet data for all active Fleetio vehicles.

Logic:
  - For each active vehicle, find the most recent run that has >= 2 .rrd snippets in S3.
  - If that run is within 7 days of today, include its snippets.
  - If no run with >= 2 snippets exists within 7 days, mark vehicle as stale.

Output: vehicles.json (read by the Go server on startup).

Usage:
    python refresh.py
    python refresh.py --restart   # also restarts the local Go server
"""

import argparse
import json
import os
import re
import subprocess
import sys
import time
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import date, datetime, timedelta, timezone

import boto3
import requests

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
FLEETIO_BASE = "https://secure.fleetio.com/api/v1"
FLEETIO_ACCOUNT_TOKEN = os.environ.get("FLEETIO_ACCOUNT_TOKEN", "a0294e2c81")
FLEETIO_API_TOKEN = os.environ.get("FLEETIO_API_TOKEN", "f9c8bdf9e92d563ba117924560478701ad17059f")

RELEVANT_MAKES = {"Nissan", "Ford", "Isuzu", "Honda"}
RELEVANT_MODELS = {"Rogue", "Mustang Mach-E", "D-MAX", "X-Trail", "Civic Hybrid"}
INACTIVE_STATUSES = {"Out of Service", "Inactive", "Build", "Verification", "Validation", "Calibration"}

# Known Fleetio registration_state values, normalized to a canonical label —
# only maps values actually observed in this fleet's data, not a general
# state/country table (avoids mis-mapping something unverified).
_LOCATION_NORMALIZE = {
    "ca": "California",
    "california": "California",
    "mi": "Michigan",
    "michigan": "Michigan",
    "fl": "Florida",
    "florida": "Florida",
    "de": "Germany",  # observed usage in this fleet refers to Germany, not Delaware
    "germany": "Germany",
    "japan": "Japan",
}


def _normalize_location(raw: str) -> str:
    if not raw:
        return ""
    return _LOCATION_NORMALIZE.get(raw.strip().lower(), raw.strip())

ADP_SQL_URL = "https://api.neuron.oci.applied.dev/api/v2/query_sql"
S3_BUCKET = "neuron-prod-data-intelligence-frame-gen"
RERUN_BASE_URL = "https://neuron.oci.applied.dev/data_explorer/v2/rerun?version=0.26&rrd="
RERUN_S3_PREFIX = f"s3://{S3_BUCKET}/rerun"

STALE_DAYS = 7
SNIPPETS_REQUIRED = 2
# Max number of recent qualifying runs to show snippets for, newest first.
MAX_RECENT_RUNS = 5
# Search ADP SQL within this many days back (wide enough for ingestion lag)
ADP_SEARCH_DAYS = 21
# Batch size for ADP OR queries (avoids huge query strings)
ADP_BATCH_SIZE = 15

# Purposes that count as real "data collection" runs — mirrors DATA_COLLECTION_MODES
# in onroad/tools/offboard/neuron/data_upload/trigger_upload_constants.py (core-stack).
DATA_COLLECTION_PURPOSES = [
    "Sensor Data Collection", "Sensor Data Collection Extended",
    "Testing: Sensor Data Collection", "Trigger Data Collection",
    "SDS Defense Sensor Data Collection", "Behavior Data Collection",
    "1stage Closed Loop with Hesai Lidar", "Calibration",
    "DCAS Pedestrian Crossing Intersection", "E2E CI Test", "NeuralSim",
]

# How long a log conversion can sit in a non-success state before we treat it as
# stuck/failed rather than just still in flight.
CONVERSION_STALE_HOURS = 48

# ordc_dashboard_runs_v2 is populated by a batch ETL job, not in real time — a
# successfully-converted run can be absent from it for well over a day before
# ingestion catches up. Only flag a run as "skipped" once it's older than this,
# to avoid false positives for runs that simply haven't been ingested yet.
SKIPPED_RUN_GRACE_HOURS = 48


# ---------------------------------------------------------------------------
# Fleetio
# ---------------------------------------------------------------------------
def get_fleetio_vehicles() -> tuple[list[dict], list[dict]]:
    """Return (active_vehicles, inactive_vehicles) matching relevant makes/models."""
    headers = {
        "Authorization": f"Token {FLEETIO_API_TOKEN}",
        "Account-Token": FLEETIO_ACCOUNT_TOKEN,
    }
    active = []
    inactive = []
    cursor = None
    while True:
        params = {"per_page": 50}
        if cursor:
            params["start_cursor"] = cursor
        resp = requests.get(f"{FLEETIO_BASE}/vehicles", headers=headers, params=params, timeout=30)
        resp.raise_for_status()
        data = resp.json()
        records = data.get("records", [])
        if not records:
            break
        for v in records:
            if v.get("make") not in RELEVANT_MAKES or v.get("model") not in RELEVANT_MODELS:
                continue
            status = (
                v.get("vehicle_status_name")
                or (v.get("vehicle_status") or {}).get("name", "")
                or v.get("status", "")
            )
            raw_name = v.get("name", "").strip()
            m = re.match(r"([A-Za-z]+)\s*-\s*(\d+)", raw_name)
            vid = f"{m.group(1).lower()}{m.group(2)}" if m else raw_name.split()[0].lower()
            overrides = {"mce101": "cosmo", "mce102": "wanda"}
            nickname = overrides.get(vid) or _extract_nickname(raw_name)
            filename_prefix = nickname if nickname else vid
            label_names = [l["name"] for l in v.get("labels") or []]
            project = _vehicle_project(label_names)
            location = _normalize_location(v.get("registration_state") or "")
            entry = {
                "vehicle_id": vid, "filename_prefix": filename_prefix, "status": status,
                "project": project, "location": location,
            }
            if status in INACTIVE_STATUSES:
                inactive.append(entry)
            else:
                active.append(entry)
        cursor = data.get("next_cursor")
        if not cursor:
            break
    return active, inactive


def _vehicle_project(labels: list[str]) -> str:
    gen = "Gen-2" if "Gen2" in labels else "Gen-1"
    if "Oasis" in labels:
        return f"Oasis {gen}"
    if "robotaxi" in labels:
        return f"Robotaxi {gen}"
    return f"Neuron {gen}"


def _extract_nickname(raw_name: str) -> str:
    m = re.search(r"\(([^)]+)\)", raw_name)
    if m:
        candidate = m.group(1).strip().lower().replace(" ", "_")
        if re.match(r"^[a-z][a-z0-9_]+$", candidate):
            return candidate
    return ""


# ---------------------------------------------------------------------------
# URSA token
# ---------------------------------------------------------------------------
def get_ursa_token() -> str:
    """Obtain a machine auth token.

    Two modes:
    1. Env vars NEURON_CLIENT_ID + NEURON_CLIENT_SECRET are set -> use them directly
       (production / Cloud Run).
    2. Otherwise fall back to AWS Secrets Manager via the 'neuron' CLI profile
       (local dev with ~/.aws credentials).
    """
    client_id = os.environ.get("NEURON_CLIENT_ID")
    client_secret = os.environ.get("NEURON_CLIENT_SECRET")

    if not (client_id and client_secret):
        # Local dev fallback: fetch creds from AWS Secrets Manager using named profile
        aws_profile_neuron = os.environ.get("AWS_PROFILE_NEURON", "neuron")
        result = subprocess.run(
            ["aws", "secretsmanager", "get-secret-value",
             "--secret-id", "neuron-machine-auth",
             "--region", "us-west-2",
             "--profile", aws_profile_neuron,
             "--query", "SecretString",
             "--output", "text"],
            capture_output=True, text=True, check=True
        )
        creds = json.loads(result.stdout.strip())
        client_id = creds["CLIENT_ID"]
        client_secret = creds["CLIENT_SECRET"]

    resp = requests.post(
        "https://accounts.applied.co/api/machineCredential/get",
        json={"client_id": client_id, "client_secret": client_secret},
        timeout=30,
    )
    resp.raise_for_status()
    return resp.json()["machine_auth_token"]


# ---------------------------------------------------------------------------
# ADP SQL
# ---------------------------------------------------------------------------
def query_adp(sql: str, token: str, limit: int = 1000, _retries: int = 3) -> list[dict]:
    """Submit a SQL query to the ADP query engine.

    Retries up to _retries times on Trino "abandoned by the client" 400 errors.
    These occur transiently when the ADP SQL service's internal Trino poller is
    overwhelmed by concurrent requests, causing Trino to mark the query abandoned.

    On retry, a unique nonce column is injected into the SELECT clause so the ADP
    service treats it as a fresh query (bypassing its result cache which would
    otherwise return the same abandoned-query error for identical SQL).
    """
    active_sql = sql
    for attempt in range(_retries):
        resp = requests.post(
            ADP_SQL_URL,
            headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
            json={"sqlQuery": active_sql, "sqlQueryOptions": {"dataSource": "ADP_QUERY_ENGINE", "limit": limit}},
            timeout=120,
        )
        if not resp.ok:
            is_abandoned = resp.status_code == 400 and "abandoned by the client" in resp.text
            is_rate_limited = resp.status_code == 429
            if (is_abandoned or is_rate_limited) and attempt < _retries - 1:
                if is_rate_limited:
                    wait = int(resp.headers.get("Retry-After", 30))
                    time.sleep(wait)
                else:
                    # Inject a unique nonce column so the ADP service treats this as a
                    # fresh query rather than returning the cached abandoned-query error.
                    nonce = str(uuid.uuid4()).replace("-", "")
                    active_sql = re.sub(
                        r"(?i)\bSELECT\b",
                        f"SELECT '{nonce}' AS _nonce,",
                        sql,
                        count=1,
                    )
                    time.sleep(5 * (attempt + 1))  # 5s, 10s backoff
                continue
            raise requests.HTTPError(
                f"ADP SQL {resp.status_code}: {resp.text[:500]}",
                response=resp,
            )
        data = resp.json()
        if data.get("message"):
            raise RuntimeError(f"ADP SQL error: {data['message']}")
        return json.loads(data.get("resultJson") or "[]")
    raise RuntimeError("query_adp: exhausted retries")



def get_latest_runs_for_vehicles(prefixes: list[str], token: str) -> dict[str, list[dict]]:
    """
    Returns {filename_prefix: [run_info, ...]} (newest first, all runs in window) for each vehicle
    that appears in mlds_unified_feature_metadata within ADP_SEARCH_DAYS.

    Batches up to ADP_BATCH_SIZE vehicles per query to minimise API calls and stay
    within the ADP SQL rate limit.
    """
    dt_from = (date.today() - timedelta(days=ADP_SEARCH_DAYS)).isoformat()
    dt_to = date.today().isoformat()

    run_map: dict[str, list[dict]] = {}
    for batch_start in range(0, len(prefixes), ADP_BATCH_SIZE):
        batch = prefixes[batch_start: batch_start + ADP_BATCH_SIZE]
        conditions = " OR ".join(
            f"customentity.segment_info.segment_id LIKE '{p}_%'" for p in batch
        )
        sql = (
            f"SELECT run_uuid, MAX(customentity.segment_info.segment_id) AS latest_seg "
            f"FROM hudi_hms.ursa_lake.mlds_unified_feature_metadata "
            f"WHERE dt >= '{dt_from}' AND dt <= '{dt_to}' "
            f"AND ({conditions}) "
            f"GROUP BY run_uuid "
            f"ORDER BY latest_seg DESC"
        )
        rows = query_adp(sql, token)
        for row in rows:
            seg = row.get("latest_seg", "")
            m = re.match(r"^([^-]+_\d{8}_\d{6})-\d+", seg)
            if not m:
                continue
            run_filename = m.group(1)
            prefix = next((p for p in batch if run_filename.startswith(p + "_")), None)
            if prefix is None:
                continue
            run_map.setdefault(prefix, []).append(
                {"run_filename": run_filename, "run_uuid": row["run_uuid"]}
            )

    for runs in run_map.values():
        runs.sort(key=lambda r: r["run_filename"], reverse=True)

    return run_map


def get_skipped_runs(vehicle_prefixes: list[str], token: str) -> dict[str, list[dict]]:
    """
    Returns {vehicle_name: [{run_uuid, drive_id, run_date, duration_minutes, reason}]}
    for runs in the last 7 days that successfully converted but never appear in
    ordc_dashboard_runs_v2 (frame gen was skipped for them). Runs younger than
    SKIPPED_RUN_GRACE_HOURS are excluded, since ordc_dashboard_runs_v2 is populated
    by a batch job and can lag well over a day behind — a recent absence there
    usually means "not ingested yet," not "actually skipped."

    Note: the real skip decision (should_run_frame_gen in post_upload_workflow.py)
    gates on segment-level validity data that lives in a separate Job Metadata
    service, not in any ADP-SQL-queryable table — so we can't report the *actual*
    reason here, only that the run never reached ORDC.
    """
    if not vehicle_prefixes:
        return {}
    dt_from = (date.today() - timedelta(days=STALE_DAYS)).isoformat()
    prefix_list = "', '".join(vehicle_prefixes)
    purpose_list = "', '".join(DATA_COLLECTION_PURPOSES)

    sql = (
        f"WITH base AS ("
        f"  SELECT CAST(d.uuid AS VARCHAR) AS run_uuid, d.vehicle_name, "
        f"         d.log_collected_at, d.duration_s "
        f"  FROM ursa_log_management.public.duration_in_seconds_view d "
        f"  JOIN ursa_log_management.public.log_conversions c ON c.source_run_uuid = d.uuid "
        f"  WHERE c.conversion_status = 2 "
        f"  AND d.vehicle_name IN ('{prefix_list}') "
        f"  AND DATE(d.log_collected_at) >= DATE('{dt_from}') "
        f"  AND d.log_collected_at <= current_timestamp - INTERVAL '{SKIPPED_RUN_GRACE_HOURS}' HOUR "
        f"  AND d.purpose IN ('{purpose_list}') "
        f"), "
        f"ordc_runs AS ("
        f"  SELECT DISTINCT run_uuid "
        f"  FROM hudi_hms.ursa_metric.ordc_dashboard_runs_v2 "
        f"  WHERE dt >= '{dt_from}' "
        f"  AND vehicle_name IN ('{prefix_list}') "
        f") "
        f"SELECT b.run_uuid, b.vehicle_name, "
        f"  CAST(b.log_collected_at AS VARCHAR) AS log_collected_at, b.duration_s "
        f"FROM base b "
        f"LEFT JOIN ordc_runs o ON o.run_uuid = b.run_uuid "
        f"WHERE o.run_uuid IS NULL "
        f"ORDER BY b.vehicle_name, b.log_collected_at DESC"
    )

    rows = query_adp(sql, token, limit=500)

    by_vehicle: dict[str, list[dict]] = {}
    for row in rows:
        vname = row["vehicle_name"]
        if vname not in by_vehicle:
            by_vehicle[vname] = []

        ts_str = row.get("log_collected_at") or ""
        try:
            dt_obj = datetime.fromisoformat(ts_str[:19])
            drive_id = f"{vname}_{dt_obj.strftime('%Y%m%d_%H%M%S')}"
            run_date = dt_obj.date().isoformat()
        except (ValueError, TypeError):
            drive_id = row["run_uuid"]
            run_date = ""

        duration_minutes = round(int(row.get("duration_s") or 0) / 60)
        if duration_minutes == 0:
            continue  # sub-minute entries are annotation/note markers, not real drives

        by_vehicle[vname].append({
            "run_uuid": row["run_uuid"],
            "drive_id": drive_id,
            "run_date": run_date,
            "duration_minutes": duration_minutes,
            "reason": "converted but never reached ORDC — exact skip reason isn't queryable via ADP SQL",
        })

    return by_vehicle


def get_conversion_issues(vehicle_prefixes: list[str], token: str) -> dict[str, list[dict]]:
    """
    Returns {vehicle_name: [{run_uuid, drive_id, run_date, duration_minutes, reason}]}
    for drives in the last 7 days whose log conversion never succeeded (no
    log_conversions row, stuck pending, or failed) and is old enough
    (> CONVERSION_STALE_HOURS) to rule out normal in-flight conversion lag.

    These drives never reach the frame-gen pipeline at all, so get_skipped_runs
    (which requires a successful conversion) can't see them — this is a distinct,
    earlier-pipeline-stage failure mode.
    """
    if not vehicle_prefixes:
        return {}
    dt_from = (date.today() - timedelta(days=STALE_DAYS)).isoformat()
    prefix_list = "', '".join(vehicle_prefixes)
    purpose_list = "', '".join(DATA_COLLECTION_PURPOSES)

    sql = (
        f"SELECT CAST(d.uuid AS VARCHAR) AS run_uuid, d.vehicle_name, "
        f"  CAST(d.log_collected_at AS VARCHAR) AS log_collected_at, d.duration_s, "
        f"  c.conversion_status "
        f"FROM ursa_log_management.public.duration_in_seconds_view d "
        f"LEFT JOIN ursa_log_management.public.log_conversions c ON c.source_run_uuid = d.uuid "
        f"WHERE d.vehicle_name IN ('{prefix_list}') "
        f"AND DATE(d.log_collected_at) >= DATE('{dt_from}') "
        f"AND d.log_collected_at <= current_timestamp - INTERVAL '{CONVERSION_STALE_HOURS}' HOUR "
        f"AND d.purpose IN ('{purpose_list}') "
        f"AND d.duration_s > 0 "
        f"AND (c.source_run_uuid IS NULL OR c.conversion_status IN (1, 3)) "
        f"ORDER BY d.vehicle_name, d.log_collected_at DESC"
    )

    rows = query_adp(sql, token, limit=500)

    # A drive can have more than one log_conversions row (e.g. multiple retried
    # conversion job attempts) — dedupe by run_uuid, keeping the worst status
    # (failed > pending) rather than emitting one entry per attempt.
    by_run_uuid: dict[str, dict] = {}
    for row in rows:
        run_uuid = row["run_uuid"]
        status = row.get("conversion_status")
        existing = by_run_uuid.get(run_uuid)
        if existing is not None and existing["_status_rank"] >= (1 if status == 3 else 0):
            continue
        by_run_uuid[run_uuid] = {"row": row, "_status_rank": 1 if status == 3 else 0}

    by_vehicle: dict[str, list[dict]] = {}
    for entry in by_run_uuid.values():
        row = entry["row"]
        vname = row["vehicle_name"]
        if vname not in by_vehicle:
            by_vehicle[vname] = []

        ts_str = row.get("log_collected_at") or ""
        try:
            dt_obj = datetime.fromisoformat(ts_str[:19])
            drive_id = f"{vname}_{dt_obj.strftime('%Y%m%d_%H%M%S')}"
            run_date = dt_obj.date().isoformat()
        except (ValueError, TypeError):
            drive_id = row["run_uuid"]
            run_date = ""

        status = row.get("conversion_status")
        if status == 1:
            reason = f"log conversion still pending after {CONVERSION_STALE_HOURS}h"
        elif status == 3:
            reason = "log conversion failed"
        else:
            reason = "never submitted for log conversion"

        duration_minutes = round(int(row.get("duration_s") or 0) / 60)
        if duration_minutes == 0:
            continue  # sub-minute entries are annotation/note markers, not real drives

        by_vehicle[vname].append({
            "run_uuid": row["run_uuid"],
            "drive_id": drive_id,
            "run_date": run_date,
            "duration_minutes": duration_minutes,
            "reason": reason,
        })

    return by_vehicle


def get_zero_segment_runs(vehicle_prefixes: list[str], token: str) -> dict[str, list[dict]]:
    """
    Returns {vehicle_name: [{run_uuid, run_id, run_date, duration_minutes,
                              filters: [{filter_name, fail_seconds, fail_pct}]}]}
    for runs in ordc_dashboard_runs_v2 in the last 7 days where usable_seconds = 0
    and recording_seconds > 0 (frame gen ran through ORDC but produced 0 usable output).
    Includes the real per-filter fail-time breakdown from ordc_dashboard_filter_metrics_v2
    (same table get_quality_runs uses) explaining why every second was filtered out.
    Sub-minute entries (annotation/note markers, not real drive segments) are excluded.
    """
    if not vehicle_prefixes:
        return {}
    dt_from = (date.today() - timedelta(days=STALE_DAYS)).isoformat()
    prefix_list = "', '".join(vehicle_prefixes)

    sql = (
        f"SELECT r.run_uuid, r.vehicle_name, r.custom_id AS run_id, r.recording_seconds, "
        f"f.filter_name, SUM(f.fail_seconds) AS total_fail_seconds "
        f"FROM hudi_hms.ursa_metric.ordc_dashboard_runs_v2 r "
        f"LEFT JOIN hudi_hms.ursa_metric.ordc_dashboard_filter_metrics_v2 f "
        f"  ON r.run_uuid = f.run_uuid AND f.dt >= '{dt_from}' AND f.fail_seconds > 0 "
        f"WHERE r.dt >= '{dt_from}' "
        f"AND r.vehicle_name IN ('{prefix_list}') "
        f"AND COALESCE(r.usable_seconds, 0) = 0 "
        f"AND COALESCE(r.recording_seconds, 0) > 0 "
        f"GROUP BY r.run_uuid, r.vehicle_name, r.custom_id, r.recording_seconds, f.filter_name "
        f"ORDER BY r.vehicle_name, r.custom_id DESC, total_fail_seconds DESC"
    )

    rows = query_adp(sql, token, limit=5000)

    # Group by (vehicle, run_id) since each run can have multiple filter rows.
    by_vehicle_run: dict[str, dict[str, dict]] = {}
    for row in rows:
        vname = row["vehicle_name"]
        run_id = row.get("run_id") or row["run_uuid"]
        if vname not in by_vehicle_run:
            by_vehicle_run[vname] = {}
        if run_id not in by_vehicle_run[vname]:
            by_vehicle_run[vname][run_id] = {
                "run_uuid": row["run_uuid"],
                "run_id": run_id,
                "recording_seconds": int(row.get("recording_seconds") or 0),
                "filters": [],
            }
        if row.get("filter_name"):
            rec_sec = by_vehicle_run[vname][run_id]["recording_seconds"]
            fail_sec = int(row.get("total_fail_seconds") or 0)
            fail_pct = round(fail_sec / rec_sec * 100, 1) if rec_sec else 0.0
            by_vehicle_run[vname][run_id]["filters"].append({
                "filter_name": row["filter_name"],
                "fail_seconds": fail_sec,
                "fail_pct": fail_pct,
            })

    by_vehicle: dict[str, list[dict]] = {}
    for vname, runs_dict in by_vehicle_run.items():
        entries = []
        for run in runs_dict.values():
            duration_minutes = round(run["recording_seconds"] / 60)
            if duration_minutes == 0:
                continue  # sub-minute entries are annotation/note markers, not real drives
            run_id = run["run_id"]
            m = re.match(r"^[^_]+_(\d{4})(\d{2})(\d{2})_", run_id)
            run_date = f"{m.group(1)}-{m.group(2)}-{m.group(3)}" if m else ""
            entries.append({
                "run_uuid": run["run_uuid"],
                "run_id": run_id,
                "run_date": run_date,
                "duration_minutes": duration_minutes,
                "filters": sorted(run["filters"], key=lambda f: f["fail_seconds"], reverse=True),
            })
        entries.sort(key=lambda r: r["run_id"], reverse=True)
        by_vehicle[vname] = entries

    return by_vehicle


def get_quality_runs(vehicle_prefixes: list[str], token: str) -> dict[str, list[dict]]:
    """
    Returns {vehicle_name: [{run_id, run_uuid, run_minutes, filters: [{filter_name, fail_seconds, fail_pct}]}]}
    for all runs in the last 7 days, sorted runs newest-first, filters by fail_pct DESC.
    Excludes location_in_garage, record_pause, slam_quality, and geofence filters.
    """
    if not vehicle_prefixes:
        return {}
    dt_from = (date.today() - timedelta(days=STALE_DAYS)).isoformat()
    prefix_list = "', '".join(vehicle_prefixes)
    sql = (
        f"SELECT r.vehicle_name, r.custom_id AS run_id, r.run_uuid, r.recording_seconds, "
        f"f.filter_name, SUM(f.fail_seconds) AS total_fail_seconds "
        f"FROM hudi_hms.ursa_metric.ordc_dashboard_runs_v2 r "
        f"JOIN hudi_hms.ursa_metric.ordc_dashboard_filter_metrics_v2 f ON r.run_uuid = f.run_uuid "
        f"WHERE r.dt >= '{dt_from}' AND f.dt >= '{dt_from}' "
        f"AND r.vehicle_name IN ('{prefix_list}') "
        f"AND f.fail_seconds > 0 "
        f"AND f.filter_name != 'location_in_garage' "
        f"AND f.filter_name != 'record_pause' "
        f"AND f.filter_name NOT LIKE '%slam_quality%' "
        f"AND f.filter_name NOT LIKE '%geofence%' "
        f"GROUP BY r.vehicle_name, r.custom_id, r.run_uuid, r.recording_seconds, f.filter_name "
        f"ORDER BY r.vehicle_name, r.custom_id DESC, total_fail_seconds DESC"
    )
    rows = query_adp(sql, token, limit=10000)

    # Group: vehicle_name -> run_id -> run_info
    by_vehicle: dict[str, dict[str, dict]] = {}
    for row in rows:
        vname = row["vehicle_name"]
        run_id = row["run_id"]
        if vname not in by_vehicle:
            by_vehicle[vname] = {}
        if run_id not in by_vehicle[vname]:
            rec_sec = int(row.get("recording_seconds") or 0)
            by_vehicle[vname][run_id] = {
                "run_id": run_id,
                "run_uuid": row["run_uuid"],
                "run_minutes": round(rec_sec / 60) if rec_sec else 0,
                "recording_seconds": rec_sec,
                "filters": [],
            }
        rec_sec = by_vehicle[vname][run_id]["recording_seconds"]
        fail_sec = int(row.get("total_fail_seconds") or 0)
        fail_pct = round(fail_sec / rec_sec * 100, 1) if rec_sec else 0.0
        by_vehicle[vname][run_id]["filters"].append({
            "filter_name": row["filter_name"],
            "fail_seconds": fail_sec,
            "fail_pct": fail_pct,
        })

    # Sort runs newest-first; drop internal recording_seconds key
    final: dict[str, list[dict]] = {}
    for vname, runs_dict in by_vehicle.items():
        runs = sorted(runs_dict.values(), key=lambda r: r["run_id"], reverse=True)
        for run in runs:
            run.pop("recording_seconds", None)
        final[vname] = runs
    return final


# ---------------------------------------------------------------------------
# S3
# ---------------------------------------------------------------------------
def _get_s3_client():
    """Return a boto3 S3 client.

    Two modes:
    1. Env vars OCI_S3_ACCESS_KEY_ID + OCI_S3_SECRET_ACCESS_KEY + OCI_S3_ENDPOINT_URL
       are set -> use explicit credentials (production / Cloud Run).
    2. Otherwise fall back to the 'oci.phx' named AWS profile (local dev).
    """
    access_key = os.environ.get("OCI_S3_ACCESS_KEY_ID")
    secret_key = os.environ.get("OCI_S3_SECRET_ACCESS_KEY")
    endpoint_url = os.environ.get("OCI_S3_ENDPOINT_URL")

    if access_key and secret_key:
        kwargs = {
            "aws_access_key_id": access_key,
            "aws_secret_access_key": secret_key,
        }
        if endpoint_url:
            kwargs["endpoint_url"] = endpoint_url
        return boto3.client("s3", **kwargs)

    # Local dev fallback: use named profile
    aws_profile_s3 = os.environ.get("AWS_PROFILE_S3", "oci.phx")
    session = boto3.Session(profile_name=aws_profile_s3)
    return session.client("s3")


def list_rrd_files(run_uuid: str) -> list[str]:
    """List .rrd files in s3://bucket/rerun/<uuid>/."""
    s3 = _get_s3_client()
    prefix = f"rerun/{run_uuid}/"
    try:
        resp = s3.list_objects_v2(Bucket=S3_BUCKET, Prefix=prefix)
    except Exception:
        return []
    files = []
    for obj in resp.get("Contents", []):
        key = obj["Key"]
        if key.endswith(".rrd"):
            files.append(key.split("/")[-1])
    return files


def run_filename_to_date(run_filename: str) -> date:
    """Extract date from run filename like rog101_20260612_131514."""
    m = re.search(r"_(\d{4})(\d{2})(\d{2})_", run_filename)
    if not m:
        return date.min
    return date(int(m.group(1)), int(m.group(2)), int(m.group(3)))


# ---------------------------------------------------------------------------
# Calibration
# ---------------------------------------------------------------------------
CALIBRATION_BUCKET = "neuron-calibrations"
CALIBRATION_CAMERAS = [
    "front_center", "front_center_narrow", "front_left", "front_right",
    "rear_center", "rear_left", "rear_right",
]
# Days since last calibration before a camera is flagged as overdue.
CALIBRATION_STALE_DAYS = 30


def _latest_calibration_date(s3, vehicle_prefix: str) -> str | None:
    """Return the latest YYYY-MM-DD calibration session folder for a vehicle, or None."""
    paginator = s3.get_paginator("list_objects_v2")
    dates = []
    for page in paginator.paginate(Bucket=CALIBRATION_BUCKET, Prefix=f"{vehicle_prefix}/", Delimiter="/"):
        for p in page.get("CommonPrefixes", []):
            parts = p["Prefix"].rstrip("/").split("/")
            if len(parts) == 2 and re.match(r"^\d{4}-\d{2}-\d{2}$", parts[1]):
                dates.append(parts[1])
    return max(dates) if dates else None


def _parse_camera_calibration(text: str) -> dict[str, int]:
    """
    Parse a -transforms.txtpb file, returning {camera_name: unix_seconds} for each
    known camera's extrinsic calibration to the lidar. Each static_transforms entry
    carries its own independent calibration_metadata.timestamp — cameras recalibrated
    independently of each other show different timestamps in the same file, so no
    diffing across historical sessions is needed to get a true per-camera date.
    """
    result = {}
    for block in text.split("static_transforms {")[1:]:
        sf = re.search(r'source_frame:\s*"([^"]+)"', block)
        tf = re.search(r'target_frame:\s*"([^"]+)"', block)
        ts = re.search(r"calibration_metadata\s*\{\s*timestamp\s*\{\s*seconds:\s*(\d+)", block)
        if not (sf and tf and ts):
            continue
        if sf.group(1) in CALIBRATION_CAMERAS and tf.group(1) == "hesai_pandar_lidar":
            result[sf.group(1)] = int(ts.group(1))
    return result


def _calibration_for_vehicle(prefix: str) -> tuple[str, list[dict]]:
    s3 = _get_s3_client()
    today = date.today()
    try:
        latest_date = _latest_calibration_date(s3, prefix)
        if not latest_date:
            return prefix, []
        # Most vehicles nest results under a "results/" subfolder, but some
        # (e.g. newly onboarded ones) write the transforms file directly under
        # the date folder — try both layouts.
        candidate_keys = [
            f"{prefix}/{latest_date}/results/{latest_date}-transforms.txtpb",
            f"{prefix}/{latest_date}/{latest_date}-transforms.txtpb",
        ]
        text = None
        last_err = None
        for key in candidate_keys:
            try:
                obj = s3.get_object(Bucket=CALIBRATION_BUCKET, Key=key)
                text = obj["Body"].read().decode("utf-8")
                break
            except Exception as e:
                last_err = e
        if text is None:
            raise last_err
        cam_timestamps = _parse_camera_calibration(text)
    except Exception as e:
        print(f"  Warning: calibration lookup failed for {prefix}: {e}")
        return prefix, []

    entries = []
    for cam in CALIBRATION_CAMERAS:
        secs = cam_timestamps.get(cam)
        if secs is None:
            continue
        cal_date = datetime.fromtimestamp(secs, tz=timezone.utc).date()
        days_since = (today - cal_date).days
        entries.append({
            "camera": cam,
            "last_calibrated": cal_date.isoformat(),
            "days_since": days_since,
            "stale": days_since > CALIBRATION_STALE_DAYS,
        })
    entries.sort(key=lambda e: e["days_since"], reverse=True)
    return prefix, entries


def get_calibration_status(vehicle_prefixes: list[str]) -> dict[str, list[dict]]:
    """
    Returns {vehicle_name: [{camera, last_calibrated, days_since, stale}]},
    sorted most-overdue first, from each vehicle's latest calibration session.
    """
    by_vehicle: dict[str, list[dict]] = {}
    with ThreadPoolExecutor(max_workers=10) as executor:
        futures = [executor.submit(_calibration_for_vehicle, p) for p in vehicle_prefixes]
        for future in as_completed(futures):
            prefix, entries = future.result()
            if entries:
                by_vehicle[prefix] = entries
    return by_vehicle


# ---------------------------------------------------------------------------
# Main refresh
# ---------------------------------------------------------------------------
def refresh_vehicle(vehicle: dict, run_candidates: list[dict] | None) -> dict:
    vid = vehicle["vehicle_id"]
    project = vehicle.get("project", "Neuron Gen-1")
    location = vehicle.get("location", "")

    if not run_candidates:
        return {"id": vid, "project": project, "location": location, "run": "", "cam_gen": "n/a", "snippets": [], "recent_runs": [], "stale": True, "stale_reason": "no run found in ADP", "inactive": False}

    # Collect up to MAX_RECENT_RUNS newest runs regardless of age or snippet
    # count so stale vehicles still show their last known runs in the UI.
    # Freshness (stale=False) still requires the newest collected run to be
    # within STALE_DAYS and have >= SNIPPETS_REQUIRED rendered snippets.
    recent_runs = []
    first_fresh = None
    last_reason = "no run found in ADP"

    for run_info in run_candidates:
        if len(recent_runs) >= MAX_RECENT_RUNS:
            break

        run_filename = run_info["run_filename"]
        run_uuid = run_info["run_uuid"]
        run_date = run_filename_to_date(run_filename)
        age_days = (date.today() - run_date).days

        rrd_files = list_rrd_files(run_uuid)
        snippets = [
            {"label": f"Snippet {i+1}", "url": f"{RERUN_BASE_URL}{RERUN_S3_PREFIX}/{run_uuid}/{f}"}
            for i, f in enumerate(rrd_files[:3])
        ]
        recent_runs.append({
            "run": run_filename,
            "run_uuid": run_uuid,
            "run_date": run_date.isoformat(),
            "snippets": snippets,
        })

        if age_days > STALE_DAYS:
            last_reason = f"last run {age_days}d ago"
        elif len(rrd_files) >= SNIPPETS_REQUIRED:
            if first_fresh is None:
                first_fresh = recent_runs[-1]
        else:
            last_reason = f"only {len(rrd_files)} snippet(s) rendered"

    if first_fresh is not None:
        return {
            "id": vid, "project": project, "location": location,
            "run": first_fresh["run"], "run_uuid": first_fresh["run_uuid"], "cam_gen": "2",
            "snippets": first_fresh["snippets"], "recent_runs": recent_runs,
            "stale": False, "inactive": False,
        }

    newest = run_candidates[0]
    return {
        "id": vid, "project": project, "location": location,
        "run": newest["run_filename"], "cam_gen": "n/a",
        "snippets": [], "recent_runs": recent_runs,
        "stale": True, "stale_reason": last_reason, "inactive": False,
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--restart", action="store_true", help="Restart local Go server after refresh")
    parser.add_argument("--dry-run", action="store_true", help="Print result without writing vehicles.json")
    args = parser.parse_args()

    print("Fetching vehicles from Fleetio...")
    vehicles, inactive_vehicles = get_fleetio_vehicles()
    print(f"  {len(vehicles)} active, {len(inactive_vehicles)} inactive")

    print("Getting URSA token...")
    token = get_ursa_token()

    print(f"Querying ADP SQL for latest runs (last {ADP_SEARCH_DAYS} days)...")
    prefixes = [v["filename_prefix"] for v in vehicles]
    run_map = get_latest_runs_for_vehicles(prefixes, token)
    print(f"  Found runs for {len(run_map)}/{len(vehicles)} vehicles")

    print("Checking S3 for .rrd snippets...")
    results = []
    # Use threads for parallel S3 listing (one per vehicle)
    with ThreadPoolExecutor(max_workers=10) as executor:
        futures = {
            executor.submit(refresh_vehicle, v, run_map.get(v["filename_prefix"])): v
            for v in vehicles
        }
        for future in as_completed(futures):
            results.append(future.result())

    # Append inactive fleet vehicles — no ADP/S3 queries needed
    for v in inactive_vehicles:
        results.append({
            "id": v["vehicle_id"],
            "project": v.get("project", "Neuron Gen-1"),
            "location": v.get("location", ""),
            "run": "",
            "cam_gen": "n/a",
            "snippets": [],
            "recent_runs": [],
            "stale": True,
            "stale_reason": f"not active in fleet ({v['status']})",
            "inactive": True,
            "quality_runs": [],
            "skipped_runs": [],
            "zero_segment_runs": [],
            "conversion_issues": [],
            "calibrations": [],
        })

    # Sort by vehicle_id
    results.sort(key=lambda r: r["id"])

    fresh = [r for r in results if not r["stale"]]
    stale = [r for r in results if r["stale"]]
    print(f"\nResults: {len(fresh)} fresh, {len(stale)} stale")

    print("Fetching quality runs from ORDC dashboard (last 7 days)...")
    # Include inactive/OOS vehicles too — a vehicle can go OOS precisely because of
    # an issue that happened in the last 7 days, so its quality/frame-gen signals
    # should still surface even though it's no longer "active" in Fleetio.
    all_fleet_vehicles = vehicles + inactive_vehicles
    prefix_to_vid = {v["filename_prefix"]: v["vehicle_id"] for v in all_fleet_vehicles}
    vid_to_prefix = {v["vehicle_id"]: v["filename_prefix"] for v in all_fleet_vehicles}
    try:
        quality_runs = get_quality_runs(list(prefix_to_vid.keys()), token)
        run_count = sum(len(v) for v in quality_runs.values())
        print(f"  {run_count} run(s) with quality data across {len(quality_runs)} vehicle(s)")
    except Exception as e:
        print(f"  Warning: quality_runs query failed: {e}")
        quality_runs = {}
    for r in results:
        prefix = vid_to_prefix.get(r["id"], r["id"])
        r["quality_runs"] = quality_runs.get(prefix, [])

    print("Fetching frame gen issues (last 7 days)...")
    try:
        skipped_runs = get_skipped_runs(list(prefix_to_vid.keys()), token)
        skip_count = sum(len(v) for v in skipped_runs.values())
        print(f"  {skip_count} pre-ORDC skipped run(s) across {len(skipped_runs)} vehicle(s)")
    except Exception as e:
        print(f"  Warning: skipped_runs query failed: {e}")
        skipped_runs = {}

    try:
        zero_seg_runs = get_zero_segment_runs(list(prefix_to_vid.keys()), token)
        zero_seg_count = sum(len(v) for v in zero_seg_runs.values())
        print(f"  {zero_seg_count} ORDC zero-valid-segment run(s) across {len(zero_seg_runs)} vehicle(s)")
    except Exception as e:
        print(f"  Warning: zero_segment_runs query failed: {e}")
        zero_seg_runs = {}

    try:
        conversion_issues = get_conversion_issues(list(prefix_to_vid.keys()), token)
        conversion_count = sum(len(v) for v in conversion_issues.values())
        print(f"  {conversion_count} stuck/failed log conversion(s) across {len(conversion_issues)} vehicle(s)")
    except Exception as e:
        print(f"  Warning: conversion_issues query failed: {e}")
        conversion_issues = {}

    print("Fetching calibration status...")
    try:
        calibration_status = get_calibration_status(list(prefix_to_vid.keys()))
        stale_cam_count = sum(1 for v in calibration_status.values() for c in v if c["stale"])
        print(f"  {stale_cam_count} overdue camera(s) across {len(calibration_status)} vehicle(s)")
    except Exception as e:
        print(f"  Warning: calibration_status query failed: {e}")
        calibration_status = {}

    # Store each type separately so the frontend can display them in distinct sections:
    #   skipped_runs        → pre-ORDC skips: run converted but never entered frame gen
    #   zero_segment_runs   → in-ORDC: frame gen ran but produced 0 valid segments
    #   conversion_issues   → never reached conversion pipeline at all (upstream of the above)
    #   calibrations        → per-camera last-calibrated dates + staleness
    for r in results:
        prefix = vid_to_prefix.get(r["id"], r["id"])
        r["skipped_runs"] = skipped_runs.get(prefix, [])
        r["zero_segment_runs"] = zero_seg_runs.get(prefix, [])
        r["conversion_issues"] = conversion_issues.get(prefix, [])
        r["calibrations"] = calibration_status.get(prefix, [])

    total_issues = sum(
        len(r["skipped_runs"]) + len(r["zero_segment_runs"]) + len(r["conversion_issues"])
        for r in results
    )
    print(f"  {total_issues} total frame gen issue run(s) across all vehicles")

    for r in stale:
        print(f"  STALE  {r['id']}: {r.get('stale_reason', '')}")

    if args.dry_run:
        print(json.dumps(results, indent=2))
        return

    out_path = os.path.join(os.path.dirname(__file__), "vehicles.json")
    with open(out_path, "w") as f:
        json.dump(results, f, indent=2)
    print(f"\nWrote {out_path}")

    if args.restart:
        print("Restarting local Go server...")
        subprocess.run(["pkill", "-f", "go run main.go"], capture_output=True)
        # Kill whatever is actually bound to :8082 (go run spawns a child binary
        # at /tmp/go-build.../exe/main which pkill -f "main.*8082" does not match)
        subprocess.run(
            "lsof -ti:8082 | xargs kill -9 2>/dev/null || true",
            shell=True, capture_output=True,
        )
        import time; time.sleep(1)
        subprocess.Popen(
            ["go", "run", "main.go"],
            cwd=os.path.dirname(__file__),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        time.sleep(2)
        print("Server restarted on :8082")


if __name__ == "__main__":
    main()
