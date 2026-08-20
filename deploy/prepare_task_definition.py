#!/usr/bin/env python3
import json
import os
import sys
from pathlib import Path


DISALLOWED_KEYS = {
    "taskDefinitionArn",
    "revision",
    "status",
    "requiresAttributes",
    "compatibilities",
    "registeredAt",
    "registeredBy",
    "deregisteredAt",
}

PASSTHROUGH_ENV_VARS = [
    # Drip SES pins: (brand=isp=profileUUID) triples routing throttled-ISP drip
    # waves via the brands' SES tenants (bypasses the PMTA throttle deferral —
    # 2026-08-05 outage). Set in the deploy shell on first rollout; inherited
    # by subsequent deploys once the task definition holds it.
    "PARTNER_DRIP_BRAND_ISP_SES_PROFILES",
    # Only set after apex-postgres-read is healthy and streaming. Empty/unset
    # means SegmentRefreshWorker uses the primary (safe default post Sev-1).
    "READ_REPLICA_URL",
    "PMTA_SSH_KEY",
    "PMTA_SSH_PASSPHRASE",
    "ANTHROPIC_API_KEY",
    # Everflow conversions export pull (Empire flooring F5). The hardcoded
    # legacy key in internal/api/everflow_conversions.go is REJECTED by
    # api.eflow.team (verified 2026-08-06: 401 on both network and affiliate
    # endpoints; mailing_everflow_conversions has ZERO source='export' rows
    # ever) — the sync scheduler stays dark until a valid affiliate API key is
    # set here once in the deploy shell, after which the task definition
    # inherits it.
    "EVERFLOW_API_KEY",
    # Campaign-lateness SMS pager (see internal/worker/campaign_health_monitor.go).
    # All five must be present in the deploy shell on first rollout; once the
    # ECS task definition holds them, subsequent deploys inherit them and these
    # can be left unset unless rotating credentials or changing recipients.
    "TWILIO_ACCOUNT_SID",
    "TWILIO_AUTH_TOKEN",
    "TWILIO_FROM_NUMBER",
    "TWILIO_TO_NUMBERS",
    "ALERT_CAMPAIGN_LATENESS_ENABLED",
    "ALERT_CAMPAIGN_LATENESS_THRESHOLD_MINUTES",
    "ALERT_CAMPAIGN_LATENESS_REALERT_HOURS",
    # Worker-stall alerts (WorkerHealthMonitor -> notify.FromEnv). Set
    # SLACK_WEBHOOK_URL in the deploy shell on first rollout; once in the task
    # definition, subsequent deploys inherit it. SLACK_ALERT_CHANNEL is only
    # used by the bot-token transport, not the webhook.
    "SLACK_WEBHOOK_URL",
    "SLACK_ALERT_CHANNEL",
    # Per-pager operational Slack channels (Twilio SMS retired 2026-06-07).
    # Defaults are baked into cmd/server/main.go (#outbox-self-check,
    # #storage-guard, #worker-stall, #campaign-lateness-pager); these env vars
    # only override the channel without a code change. All post via SLACK_BOT_TOKEN.
    "SLACK_OUTBOX_CHANNEL",
    "SLACK_STORAGE_GUARD_CHANNEL",
    "SLACK_WORKER_STALL_CHANNEL",
    "SLACK_CAMPAIGN_LATENESS_CHANNEL",
    # Conversion alerts → #conversions Slack channel (Everflow conversion
    # postbacks). The active transport is the shared "Jarvis Maintenance Workers"
    # bot token (SLACK_BOT_TOKEN, scopes chat:write + chat:write.public) which
    # notify.ConversionsFromEnv() posts to SLACK_CONVERSIONS_CHANNEL (#conversions).
    # SLACK_CONVERSIONS_WEBHOOK_URL is an alternative incoming-webhook transport
    # (none currently bound to #conversions). Set in the deploy shell on first
    # rollout; once in the task definition, subsequent deploys inherit it.
    "SLACK_BOT_TOKEN",
    "SLACK_CONVERSIONS_WEBHOOK_URL",
    "SLACK_CONVERSIONS_CHANNEL",
    # Kafka event backbone (internal/eventbus). DARK unless KAFKA_BROKERS is set
    # AND the per-flow KAFKA_FLAG_PRODUCE_* is ON. Set in the deploy shell on the
    # rollout that activates the bus; once in the task def, subsequent deploys
    # inherit them. KAFKA_ALLOW_AUTO_TOPICS lets the producer create evt.* topics
    # on first publish (bootstrap/test against MSK Serverless).
    "KAFKA_BROKERS",
    "KAFKA_SASL_MECHANISM",
    "KAFKA_TLS",
    "KAFKA_ALLOW_AUTO_TOPICS",
    "KAFKA_FLAG_PRODUCE_LAKE",
    "KAFKA_FLAG_PRODUCE_INGEST",
    "KAFKA_FLAG_PRODUCE_SUPPRESS",
    "ANALYTICS_FIREHOSE_STREAM_SHADOW",
    # SK-4 Kafka-PRIMARY send queue (routes real sends through Kafka). DARK unless
    # KAFKA_SEND_QUEUE_ENABLED warms the consumer AND a routing allowlist (or _ALL)
    # selects waves. Default unset ⇒ EnqueuePMTAWave routes nothing ⇒ byte-identical.
    "KAFKA_SEND_QUEUE_ENABLED",
    "KAFKA_SEND_QUEUE_ALL",
    "KAFKA_SEND_QUEUE_WAVES",
    "KAFKA_SEND_QUEUE_CAMPAIGNS",
]

REMOVE_ENV_VARS = [
    "DB_ADMIN_URL",
    "PARTNER_DRIP_FOLLOWUP_DISABLED",
]


def upsert_env(env_list, name, value):
    for item in env_list:
        if item.get("name") == name:
            item["value"] = value
            return
    env_list.append({"name": name, "value": value})


def main() -> int:
    if len(sys.argv) != 8:
        print(
            "usage: prepare_task_definition.py <input> <output> <container_name> <image_uri> <git_sha> <build_time> <image_digest>",
            file=sys.stderr,
        )
        return 1

    input_path = Path(sys.argv[1])
    output_path = Path(sys.argv[2])
    container_name = sys.argv[3]
    image_uri = sys.argv[4]
    git_sha = sys.argv[5]
    build_time = sys.argv[6]
    image_digest = sys.argv[7]

    payload = json.loads(input_path.read_text())
    task_def = payload.get("taskDefinition", payload)
    sanitized = {key: value for key, value in task_def.items() if key not in DISALLOWED_KEYS}

    containers = sanitized.get("containerDefinitions", [])
    target = None
    for container in containers:
        if container.get("name") == container_name:
            target = container
            break

    if target is None:
        raise SystemExit(f"container {container_name!r} not found in task definition")

    target["image"] = image_uri
    env_list = target.setdefault("environment", [])
    upsert_env(env_list, "APP_VERSION", git_sha)
    upsert_env(env_list, "APP_GIT_SHA", git_sha)
    upsert_env(env_list, "APP_BUILD_TIME", build_time)
    upsert_env(env_list, "APP_IMAGE_URI", image_uri)
    upsert_env(env_list, "APP_IMAGE_DIGEST", image_digest)

    upsert_env(env_list, "JARVIS_S3_BUCKET", "jarvis-image-cdn")
    upsert_env(env_list, "JARVIS_S3_REGION", "us-west-2")
    upsert_env(env_list, "IMAGE_CDN_DOMAIN", "img.projectjarvis.io")
    upsert_env(env_list, "ENABLE_ENGAGEMENT_ESCALATION", "true")

    # Secrets never live in this file: DEPLOY_UPSERT_ENVS names env vars whose
    # VALUES come from the deploy shell (e.g. DEPLOY_UPSERT_ENVS=ANTHROPIC_API_KEY
    # with ANTHROPIC_API_KEY exported). Unset/empty names are skipped loudly.
    for name in filter(None, os.environ.get("DEPLOY_UPSERT_ENVS", "").split(",")):
        name = name.strip()
        value = os.environ.get(name, "")
        if value:
            upsert_env(env_list, name, value)
        else:
            print(f"WARNING: DEPLOY_UPSERT_ENVS names {name!r} but it is empty in "
                  "the deploy shell — NOT upserted", file=sys.stderr)
    # 2026-06-04: decommission the convictions/decisions engine DB persistence.
    # Stops INSERTs into mailing_engine_convictions (~37 GB) + mailing_engine_decisions
    # (~19 GB) — the dominant RDS write-IO source, never read by the send path.
    # In-memory agents, S3 conviction archival, and throttle state are unaffected.
    # Reversible: set to "false" (or remove this line) and redeploy.
    upsert_env(env_list, "ENGINE_DECISION_PERSIST_DISABLED", "true")
    upsert_env(env_list, "SUPPRESSION_S3_BUCKET", "jarvis-offer-suppressions")
    upsert_env(env_list, "SUPPRESSION_S3_REGION", "us-west-2")
    upsert_env(env_list, "REDIS_URL", "apex-redis.x9k9ng.0001.usw2.cache.amazonaws.com:6379")
    upsert_env(env_list, "DISABLE_ISP_RATE_LIMITING", "true")
    # Partner drip phase 2: follow-ups on; bypass stale ISP throttle deferrals.
    upsert_env(env_list, "PARTNER_DRIP_THROTTLE_THRESHOLD", "0")
    # 2026-08-19 (operator: "I should be able to manage, view, and control the
    # drips from this body of work"). Both surfaces shipped read-only behind a
    # flag; these two lines are what actually hand the operator the levers.
    #   THROTTLE -> edit per-ISP pct_override / max_per_wave / daily_cap on a
    #     lane (property_lane_supply.go:479). Writes reuse the existing
    #     data-partners isp-distribution PUT; there is no second writer.
    #   ROSTER   -> assign/unassign a sending domain to a drip
    #     (property_lane_roster.go). partner_drip_vertical_roster is reloaded by
    #     the orchestrator EVERY TICK, so a row write changes live sending with
    #     no deploy. Unassign soft-disables (active=false); it never deletes.
    # Reversible: remove either line (or set to "0") and redeploy — the panels
    # fall back to read-only with a banner naming the env var. Only "1" enables.
    upsert_env(env_list, "PROPERTY_LEDGER_THROTTLE_WRITE_ENABLED", "1")
    upsert_env(env_list, "PROPERTY_LEDGER_ROSTER_WRITE_ENABLED", "1")
    # 2026-08-19: PG rollup worker DISABLED. Its 30-day backfill contended with
    # the request path on the same RDS instance and starved it — /property-ledger
    # /stats began returning "campaign resolution failed: canceling statement due
    # to user request" on every call. Replacement is an Athena snapshot written to
    # S3 (operator call: "It can take a snapshot from athena on a per 5 min basis
    # and builds the snapshot as a json file"), which puts the scan on the lake
    # instead of prod PG. Endpoint falls back to its live path, i.e. pre-rollup
    # behaviour. Remove this line to re-enable.
    upsert_env(env_list, "LANE_STATS_ROLLUP_DISABLED", "1")
    upsert_env(env_list, "PARTNER_DRIP_CREATIVES_DIR", "docs/emails")
    # Yahoo newsletter-only drip lane: OFF (operator 2026-08-06, Empire flooring
    # diagnosis). The 2026-07-14 newsletter-only ruling suppressed every Yahoo
    # offer *reminder* touch — the touch type that produced both known Empire
    # conversions — and the setting is global (no per-drip dimension, F9), so
    # it starved reminders across all drip lanes. Yahoo now receives offer
    # follow-ups like every other ISP. Reversible: set back to "1" and redeploy.
    upsert_env(env_list, "PARTNER_DRIP_YAHOO_NEWSLETTER_ONLY", "0")
    # Analytics event-lake READ layer (Athena). Setting ANALYTICS_ATHENA_OUTPUT
    # enables the reader (/api/mailing/analytics/lake/*) and un-darks the Event
    # Lake screen; it points at the live ignite_analytics Glue DB + Athena
    # workgroup (output bucket s3://ignite-analytics-lake/athena-results/).
    # Reversible: remove these three lines and redeploy to take read dark again.
    # The WRITE side is enabled separately via ANALYTICS_FIREHOSE_STREAM, which is
    # already on the task definition and carries forward from the base.
    upsert_env(env_list, "ANALYTICS_ATHENA_DATABASE", "ignite_analytics")
    upsert_env(env_list, "ANALYTICS_ATHENA_WORKGROUP", "ignite_analytics")
    upsert_env(env_list, "ANALYTICS_ATHENA_OUTPUT", "s3://ignite-analytics-lake/athena-results/")

    for var_name in PASSTHROUGH_ENV_VARS:
        val = os.environ.get(var_name)
        if val:
            upsert_env(env_list, var_name, val)

    env_list[:] = [e for e in env_list if e.get("name") not in REMOVE_ENV_VARS]

    # ReviewForge engine sidecar (Creative Studio). awsvpc networking shares
    # the namespace, so the Go server reaches it at localhost:3100. Set
    # REVIEW_FORGE_IMAGE in the deploy shell to add/update the sidecar (built
    # by review-forge/deploy/build-push.sh); when unset, an existing sidecar
    # definition carries forward from the base task definition unchanged.
    # essential=false: an engine crash degrades Creative Studio, not sending.
    rf_image = os.environ.get("REVIEW_FORGE_IMAGE")
    if rf_image:
        upsert_env(env_list, "REVIEW_FORGE_INTERNAL_URL", "http://localhost:3100")
        sidecar = None
        for container in containers:
            if container.get("name") == "review-forge-engine":
                sidecar = container
                break
        if sidecar is None:
            sidecar = {"name": "review-forge-engine"}
            containers.append(sidecar)
        sidecar["image"] = rf_image
        sidecar["essential"] = False
        sidecar["portMappings"] = [{"containerPort": 3100, "protocol": "tcp"}]
        sidecar_env = sidecar.setdefault("environment", [])
        upsert_env(sidecar_env, "PORT", "3100")
        # Next standalone binds to $HOSTNAME; in ECS that's the task hostname,
        # which leaves 127.0.0.1 unbound and the Go server locked out.
        upsert_env(sidecar_env, "HOSTNAME", "0.0.0.0")
        upsert_env(sidecar_env, "REVIEW_FORGE_DISABLE_LOCAL_FEEDS", "1")
        if target.get("logConfiguration") and not sidecar.get("logConfiguration"):
            log_cfg = json.loads(json.dumps(target["logConfiguration"]))
            opts = log_cfg.get("options", {})
            if "awslogs-stream-prefix" in opts:
                opts["awslogs-stream-prefix"] = opts["awslogs-stream-prefix"] + "-rf"
            sidecar["logConfiguration"] = log_cfg

    output_path.write_text(json.dumps(sanitized, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
