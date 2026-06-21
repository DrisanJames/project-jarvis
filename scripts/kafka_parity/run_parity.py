#!/usr/bin/env python3
"""
run_parity.py — Kafka event-backbone parity reconciliation runner.

READ-ONLY. Runs the three parity SQL files against the live Postgres (IGNITE_DSN)
and prints a pass/fail summary against the plan's thresholds. It writes NOTHING
to any table and is safe to run on production at any time.

What it checks
--------------
  1. ingest_parity.sql      mailing_tracking_events  vs ingest_events_shadow
                            (completeness, excess, field fidelity, dedup parity)
  2. lake_parity.sql        live lake partition       vs kafka-shadow lake partition
                            (completeness, excess, field fidelity) for a dt
  3. suppression_safety.sql suppressed_but_would_mail (MUST be 0 rows)

Thresholds (from the migration plan)
------------------------------------
  missing-rate (completeness)   <= MAX_MISSING_RATE   (default 0.0)
  field-mismatch                == 0.00%              (hard zero)
  excess                        == 0                  (hard zero)
  dedup parity                  == 0                  (hard zero)
  suppressed_but_would_mail     == 0 rows             (hard zero — safety gate)

Usage
-----
  IGNITE_DSN=postgres://... python3 run_parity.py --dt 2026-06-20
  python3 run_parity.py --dt 2026-06-20 \
      --live-lake-table live_lake_events --kafka-lake-table kafka_lake_events \
      --supp-window-start 2026-06-20 --supp-window-end 2026-06-21
  python3 run_parity.py --dt 2026-06-20 --skip-lake     # if lake projections absent

Exit code is 0 only if every gate passes, else 1 (CI / cron friendly).
"""

import argparse
import os
import sys
from datetime import datetime, timedelta

try:
    import psycopg2
    from psycopg2 import sql
    from psycopg2.extras import RealDictCursor
except ImportError:
    sys.stderr.write("psycopg2 is required (pip install psycopg2-binary)\n")
    sys.exit(2)

HERE = os.path.dirname(os.path.abspath(__file__))

# The repo's psycopg2 scripts standardize on IGNITE_DSN; accept the common
# fallbacks too so this runs in dev and CI without extra wiring.
DSN = (
    os.environ.get("IGNITE_DSN")
    or os.environ.get("MAILING_PG_DSN")
    or os.environ.get("DATABASE_URL")
)

# Plan thresholds.
MAX_MISSING_RATE = float(os.environ.get("MAX_MISSING_RATE", "0.0"))


def _read_sql(name: str) -> str:
    with open(os.path.join(HERE, name), "r") as fh:
        return fh.read()


def _fmt(n) -> str:
    return f"{n:,}" if isinstance(n, int) else str(n)


def _rate(missing: int, total: int) -> float:
    return (missing / total) if total else 0.0


def run_ingest_parity(conn, dt_start: str, dt_end: str) -> bool:
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(_read_sql("ingest_parity.sql"),
                    {"dt_start": dt_start, "dt_end": dt_end})
        r = cur.fetchone()

    live = int(r["live_rows"])
    missing_rate = _rate(int(r["missing_in_shadow"]), live)
    checks = [
        ("completeness (missing_in_shadow)", int(r["missing_in_shadow"]),
         missing_rate <= MAX_MISSING_RATE, f"rate={missing_rate:.4%} <= {MAX_MISSING_RATE:.4%}"),
        ("excess_in_shadow", int(r["excess_in_shadow"]),
         int(r["excess_in_shadow"]) == 0, "must be 0"),
        ("field_mismatches", int(r["field_mismatches"]),
         int(r["field_mismatches"]) == 0, "must be 0 (0.00%)"),
        ("dedup_parity_fail", int(r["dedup_parity_fail"]),
         int(r["dedup_parity_fail"]) == 0, "must be 0"),
    ]
    print("\n== ingest parity (mailing_tracking_events vs ingest_events_shadow) ==")
    print(f"   live_rows={_fmt(live)}  shadow_rows={_fmt(int(r['shadow_rows']))}")
    return _report(checks)


def run_lake_parity(conn, dt: str, live_tbl: str, kafka_tbl: str) -> bool:
    raw = _read_sql("lake_parity.sql")
    query = sql.SQL(raw).format(
        live_lake_table=sql.Identifier(live_tbl),
        kafka_lake_table=sql.Identifier(kafka_tbl),
    )
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(query, {"dt": dt})
        r = cur.fetchone()

    live = int(r["live_uids"])
    missing_rate = _rate(int(r["missing_in_kafka"]), live)
    checks = [
        ("completeness (missing_in_kafka)", int(r["missing_in_kafka"]),
         missing_rate <= MAX_MISSING_RATE, f"rate={missing_rate:.4%} <= {MAX_MISSING_RATE:.4%}"),
        ("excess_in_kafka", int(r["excess_in_kafka"]),
         int(r["excess_in_kafka"]) == 0, "must be 0"),
        ("field_mismatches", int(r["field_mismatches"]),
         int(r["field_mismatches"]) == 0, "must be 0 (0.00%)"),
    ]
    print(f"\n== lake parity (dt={dt}: {live_tbl} vs {kafka_tbl}) ==")
    print(f"   live_uids={_fmt(live)}  kafka_uids={_fmt(int(r['kafka_uids']))}")
    return _report(checks)


def run_suppression_safety(conn, w_start: str, w_end: str) -> bool:
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(_read_sql("suppression_safety.sql"),
                    {"window_start": w_start, "window_end": w_end})
        rows = cur.fetchall()

    n = len(rows)
    print(f"\n== suppression safety (window {w_start} .. {w_end}) ==")
    print(f"   suppressed_but_would_mail = {_fmt(n)} rows (must be 0)")
    if n:
        for ex in rows[:10]:
            print(f"     DEFECT would-mail: {ex['suppressed_but_would_mail']}")
        if n > 10:
            print(f"     ... and {n - 10} more")
    return _report([("suppressed_but_would_mail", n, n == 0, "must be EXACTLY 0 (safety gate)")])


def _report(checks) -> bool:
    ok = True
    for name, value, passed, rule in checks:
        status = "PASS" if passed else "FAIL"
        ok = ok and passed
        print(f"   [{status}] {name} = {_fmt(value)}  ({rule})")
    return ok


def main() -> int:
    if not DSN:
        sys.stderr.write("IGNITE_DSN (or MAILING_PG_DSN / DATABASE_URL) must be set\n")
        return 2

    ap = argparse.ArgumentParser(description="Kafka parity reconciliation (read-only)")
    ap.add_argument("--dt", required=True, help="partition date, e.g. 2026-06-20 (UTC)")
    ap.add_argument("--live-lake-table", default="live_lake_events",
                    help="PG projection of the live lake partition")
    ap.add_argument("--kafka-lake-table", default="kafka_lake_events",
                    help="PG projection of the kafka-shadow lake partition")
    ap.add_argument("--supp-window-start", default=None,
                    help="suppression window start (default = --dt)")
    ap.add_argument("--supp-window-end", default=None,
                    help="suppression window end (default = --dt + 1 day)")
    ap.add_argument("--skip-lake", action="store_true",
                    help="skip lake_parity (e.g. lake projections not present)")
    args = ap.parse_args()

    dt_start = args.dt
    dt_end = (datetime.strptime(args.dt, "%Y-%m-%d") + timedelta(days=1)).strftime("%Y-%m-%d")
    w_start = args.supp_window_start or dt_start
    w_end = args.supp_window_end or dt_end

    print(f"Kafka parity reconciliation @ {datetime.utcnow().isoformat()}Z")
    print(f"dt window: [{dt_start}, {dt_end})   MAX_MISSING_RATE={MAX_MISSING_RATE:.4%}")

    conn = psycopg2.connect(DSN)
    conn.set_session(readonly=True, autocommit=True)
    results = []
    try:
        results.append(("ingest_parity", run_ingest_parity(conn, dt_start, dt_end)))
        if not args.skip_lake:
            try:
                results.append(("lake_parity",
                                run_lake_parity(conn, args.dt,
                                                args.live_lake_table, args.kafka_lake_table)))
            except psycopg2.Error as e:
                conn.rollback()
                print(f"\n== lake parity SKIPPED (query error: {str(e).strip()}) ==")
                print("   (pass --skip-lake or create the lake projections)")
                results.append(("lake_parity", False))
        results.append(("suppression_safety", run_suppression_safety(conn, w_start, w_end)))
    finally:
        conn.close()

    all_ok = all(ok for _, ok in results)
    print("\n" + "=" * 60)
    print("PARITY SUMMARY")
    for name, ok in results:
        print(f"   {'PASS' if ok else 'FAIL'}  {name}")
    print(f"OVERALL: {'PASS — safe to advance the Kafka cutover' if all_ok else 'FAIL — do NOT advance cutover'}")
    print("=" * 60)
    return 0 if all_ok else 1


if __name__ == "__main__":
    sys.exit(main())
