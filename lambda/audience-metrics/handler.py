"""
Audience Metrics Lambda — computes sidebar KPIs every 15 minutes.

Writes a single row to mailing_audience_metrics:
  active_audience_60d  — unique openers + clickers (60 days, excluding seeds)
  global_churn_pct     — (unsubs + complaints in 30d) / active non-seed subscribers
  global_intro_pct     — % of 30d recipients who were first-time sends
"""

import logging
import os
from urllib.parse import urlparse, parse_qs

import pg8000.native

logger = logging.getLogger()
logger.setLevel(logging.INFO)

SEED_EXCLUSION = "SELECT id FROM mailing_lists WHERE name ILIKE '%%seed%%'"


def _connect():
    raw = os.environ["DATABASE_URL"]
    url = urlparse(raw)
    params = parse_qs(url.query)
    ssl_mode = params.get("sslmode", ["require"])[0]
    use_ssl = ssl_mode not in ("disable",)

    kwargs = dict(
        host=url.hostname,
        port=url.port or 5432,
        database=url.path.lstrip("/"),
        user=url.username,
        password=url.password,
    )
    if use_ssl:
        import ssl as _ssl

        kwargs["ssl_context"] = _ssl.create_default_context()
        kwargs["ssl_context"].check_hostname = False
        kwargs["ssl_context"].verify_mode = _ssl.CERT_NONE

    return pg8000.native.Connection(**kwargs)


def _active_audience(conn):
    """Unique subscribers who opened or clicked in the last 60 days, excluding seeds.

    Uses mailing_subscribers.last_open_at / last_click_at which are maintained
    by the tracking handlers on every open/click webhook.
    """
    rows = conn.run(f"""
        SELECT COUNT(*)
        FROM mailing_subscribers s
        WHERE s.status = 'confirmed'
          AND (s.last_open_at >= NOW() - INTERVAL '60 days'
               OR s.last_click_at >= NOW() - INTERVAL '60 days')
          AND s.list_id NOT IN ({SEED_EXCLUSION})
    """)
    return rows[0][0]


def _global_churn(conn):
    """(unsubs + complaints in 30d) / total active non-seed subscribers * 100."""
    rows = conn.run(f"""
        SELECT COUNT(DISTINCT e.subscriber_id)
        FROM mailing_tracking_events e
        JOIN mailing_subscribers s ON s.id = e.subscriber_id
        WHERE e.event_type IN ('unsubscribe', 'complained')
          AND e.event_at >= NOW() - INTERVAL '30 days'
          AND s.list_id NOT IN ({SEED_EXCLUSION})
    """)
    numerator = rows[0][0]

    rows = conn.run("""
        SELECT COALESCE(SUM(active_count), 0)
        FROM mailing_lists
        WHERE status = 'active'
          AND name NOT ILIKE '%%seed%%'
          AND COALESCE(is_visible, true) = true
    """)
    denominator = rows[0][0]

    if denominator > 0:
        return round(numerator / denominator * 100, 3)
    return 0.0


def _global_intro(conn):
    """% of 30d recipients whose first-ever sent event was within that window."""
    rows = conn.run(f"""
        WITH recent_sends AS (
            SELECT DISTINCT e.subscriber_id
            FROM mailing_tracking_events e
            JOIN mailing_subscribers s ON s.id = e.subscriber_id
            WHERE e.event_type = 'sent'
              AND e.event_at >= NOW() - INTERVAL '30 days'
              AND s.list_id NOT IN ({SEED_EXCLUSION})
        ),
        first_timers AS (
            SELECT rs.subscriber_id FROM recent_sends rs
            WHERE NOT EXISTS (
                SELECT 1 FROM mailing_tracking_events e2
                WHERE e2.subscriber_id = rs.subscriber_id
                  AND e2.event_type = 'sent'
                  AND e2.event_at < NOW() - INTERVAL '30 days'
            )
        )
        SELECT
            (SELECT COUNT(*) FROM first_timers),
            (SELECT COUNT(*) FROM recent_sends)
    """)
    intro_count, total_sent_to = rows[0]
    if total_sent_to > 0:
        return round(intro_count / total_sent_to * 100, 3)
    return 0.0


def handler(event, context):
    conn = _connect()
    try:
        active = _active_audience(conn)
        churn = _global_churn(conn)
        intro = _global_intro(conn)

        conn.run(
            "UPDATE mailing_audience_metrics "
            "SET active_audience_60d = :aa, global_churn_pct = :ch, "
            "    global_intro_pct = :ip, computed_at = NOW() "
            "WHERE id = 1",
            aa=active,
            ch=churn,
            ip=intro,
        )

        result = {
            "active_audience_60d": active,
            "global_churn_pct": churn,
            "global_intro_pct": intro,
        }
        logger.info("Audience metrics updated: %s", result)
        return result
    finally:
        conn.close()
