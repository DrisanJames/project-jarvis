#!/bin/bash
# verify_send_day_planner.sh — Phase 5 first-deploy verification protocol
# for the Send-Day Planner canvas. Per .cursor/rules/testing.mdc the
# observed-with-eyes verification is REQUIRED before declaring the
# deploy complete.
#
# This script runs the curl/psql/CloudWatch checks the plan calls for
# (Section: "Production verification + sign-off"):
#
#   1. /health git_sha matches expected
#   2. GET /api/mailing/campaigns?search=<DATE_PREFIX> returns 16 rows
#   3. psql field-by-field check each campaign row against the canvas
#      golden fixture's payload (name, sending_domain, target_isps,
#      isp_quotas, scheduled_at, status)
#   4. CloudWatch grep [PMTAWaveScheduler] confirms first-wave fire
#      within scheduled minute
#
# Usage:
#   AWS_REGION=us-west-2 \
#   PUBLIC_BASE_URL=https://projectjarvis.io \
#   EXPECTED_GIT_SHA=<sha> \
#   ORG_ID=<uuid> \
#   ADMIN_KEY=<key> \
#   DATE_PREFIX=05122026 \
#   FIXTURE_DIR=upside-down/web/src/components/mailing/components/send-day-planner/__fixtures__ \
#   PROD_DB_URL=postgres://... \
#   ./verify_send_day_planner.sh
#
# Each step prints PASS or FAIL and aggregates an exit code at the end.
# The script does NOT cancel any campaigns on failure — operator
# investigates and decides next-step manually.

set -uo pipefail

PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-https://projectjarvis.io}"
EXPECTED_GIT_SHA="${EXPECTED_GIT_SHA:?set EXPECTED_GIT_SHA to the commit you deployed}"
ORG_ID="${ORG_ID:?set ORG_ID (organization UUID for the API call)}"
ADMIN_KEY="${ADMIN_KEY:?set ADMIN_KEY (X-Admin-Key for the campaigns search endpoint)}"
DATE_PREFIX="${DATE_PREFIX:?set DATE_PREFIX (MMDDYYYY of the send-day, e.g. 05122026)}"
FIXTURE_DIR="${FIXTURE_DIR:-upside-down/web/src/components/mailing/components/send-day-planner/__fixtures__}"
PROD_DB_URL="${PROD_DB_URL:-}"
AWS_REGION="${AWS_REGION:-us-west-2}"
LOG_GROUP="${LOG_GROUP:-/ecs/ignite-upside-down}"
EXPECTED_CAMPAIGN_COUNT="${EXPECTED_CAMPAIGN_COUNT:-16}"

PASS=0
FAIL=0
REPORT=()

note() {
  printf '\n— %s\n' "$1"
}
ok() {
  printf '  PASS · %s\n' "$1"
  PASS=$((PASS + 1))
  REPORT+=("PASS · $1")
}
err() {
  printf '  FAIL · %s\n' "$1"
  FAIL=$((FAIL + 1))
  REPORT+=("FAIL · $1")
}

# ─── 1. /health git_sha ──────────────────────────────────────────────────────
note "Step 1 · GET ${PUBLIC_BASE_URL}/health — confirm deployed git_sha"
HEALTH_JSON=$(curl -sS "${PUBLIC_BASE_URL}/health" || true)
if [ -z "${HEALTH_JSON}" ]; then
  err "/health returned no body"
else
  GIT_SHA=$(printf '%s' "${HEALTH_JSON}" | jq -r '.build.git_sha // .git_sha // empty')
  if [ -z "${GIT_SHA}" ]; then
    err "/health missing build.git_sha"
  elif [[ "${GIT_SHA}" == "${EXPECTED_GIT_SHA}"* ]]; then
    ok "/health git_sha=${GIT_SHA} matches expected ${EXPECTED_GIT_SHA}"
  else
    err "/health git_sha=${GIT_SHA} does NOT contain ${EXPECTED_GIT_SHA}"
  fi
fi

# ─── 2. /api/mailing/campaigns?search=<DATE_PREFIX> — 16 campaigns ──────────
note "Step 2 · search campaigns by date prefix ${DATE_PREFIX}"
CAMPAIGNS_JSON=$(curl -sS \
  -H "X-Organization-ID: ${ORG_ID}" \
  -H "X-Admin-Key: ${ADMIN_KEY}" \
  "${PUBLIC_BASE_URL}/api/mailing/campaigns?search=${DATE_PREFIX}" || true)
if [ -z "${CAMPAIGNS_JSON}" ]; then
  err "/api/mailing/campaigns returned no body"
else
  COUNT=$(printf '%s' "${CAMPAIGNS_JSON}" | jq -r '(.campaigns // .) | length')
  if [ "${COUNT}" = "${EXPECTED_CAMPAIGN_COUNT}" ]; then
    ok "Found ${COUNT} campaigns matching ${DATE_PREFIX} (expected ${EXPECTED_CAMPAIGN_COUNT})"
  else
    err "Expected ${EXPECTED_CAMPAIGN_COUNT} campaigns, found ${COUNT}"
  fi
fi

# ─── 3. psql field-by-field against goldens ─────────────────────────────────
note "Step 3 · field-by-field check against canvas golden fixtures"
if [ -z "${PROD_DB_URL}" ]; then
  err "PROD_DB_URL not set; skipping psql verification (manual step required)"
elif ! command -v psql >/dev/null 2>&1; then
  err "psql binary not on PATH; skipping (manual step required)"
elif [ ! -d "${FIXTURE_DIR}" ]; then
  err "FIXTURE_DIR does not exist: ${FIXTURE_DIR}"
else
  GOLDEN_FILES=$(ls "${FIXTURE_DIR}"/*.post.json 2>/dev/null | sort)
  if [ -z "${GOLDEN_FILES}" ]; then
    err "no .post.json fixtures found in ${FIXTURE_DIR}"
  else
    GOLDEN_COUNT=0
    GOLDEN_OK=0
    while IFS= read -r f; do
      GOLDEN_COUNT=$((GOLDEN_COUNT + 1))
      NAME=$(jq -r '.name' "$f")
      EXPECTED_DOMAIN=$(jq -r '.sending_domain' "$f")
      EXPECTED_TGT_COUNT=$(jq -r '.target_isps | length' "$f")
      EXPECTED_QUOTA_SUM=$(jq -r '.isp_quotas | map(.volume) | add' "$f")
      EXPECTED_SCHEDULED=$(jq -r '.scheduled_at' "$f")
      ROW=$(psql "${PROD_DB_URL}" -At -F '|' -c \
        "SELECT sending_domain, COALESCE(jsonb_array_length(target_isps),0), COALESCE((SELECT SUM((q->>'volume')::int) FROM jsonb_array_elements(isp_quotas) q),0), to_char(scheduled_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'), status FROM mailing_campaigns WHERE name = '${NAME//\'/\'\'}' LIMIT 1")
      if [ -z "${ROW}" ]; then
        err "[$(basename "$f")] no DB row for name=${NAME}"
        continue
      fi
      ACT_DOMAIN=$(printf '%s' "${ROW}" | cut -d'|' -f1)
      ACT_TGT_COUNT=$(printf '%s' "${ROW}" | cut -d'|' -f2)
      ACT_QUOTA_SUM=$(printf '%s' "${ROW}" | cut -d'|' -f3)
      ACT_SCHEDULED=$(printf '%s' "${ROW}" | cut -d'|' -f4)
      ACT_STATUS=$(printf '%s' "${ROW}" | cut -d'|' -f5)
      MISMATCH=""
      [ "${ACT_DOMAIN}" = "${EXPECTED_DOMAIN}" ]   || MISMATCH+="domain(${ACT_DOMAIN}!=${EXPECTED_DOMAIN}) "
      [ "${ACT_TGT_COUNT}" = "${EXPECTED_TGT_COUNT}" ] || MISMATCH+="target_isps(${ACT_TGT_COUNT}!=${EXPECTED_TGT_COUNT}) "
      [ "${ACT_QUOTA_SUM}" = "${EXPECTED_QUOTA_SUM}" ] || MISMATCH+="quota_sum(${ACT_QUOTA_SUM}!=${EXPECTED_QUOTA_SUM}) "
      [ "${ACT_SCHEDULED}" = "${EXPECTED_SCHEDULED}" ] || MISMATCH+="scheduled_at(${ACT_SCHEDULED}!=${EXPECTED_SCHEDULED}) "
      case "${ACT_STATUS}" in
        scheduled|preparing|finalizing_audience|sending|sent) ;;
        *) MISMATCH+="status(${ACT_STATUS}) " ;;
      esac
      if [ -z "${MISMATCH}" ]; then
        GOLDEN_OK=$((GOLDEN_OK + 1))
      else
        err "[$(basename "$f")] mismatch: ${MISMATCH}"
      fi
    done <<< "${GOLDEN_FILES}"
    if [ "${GOLDEN_OK}" = "${GOLDEN_COUNT}" ] && [ "${GOLDEN_COUNT}" -gt 0 ]; then
      ok "${GOLDEN_OK}/${GOLDEN_COUNT} golden fixtures byte-equal to DB rows"
    elif [ "${GOLDEN_OK}" -gt 0 ]; then
      err "Only ${GOLDEN_OK}/${GOLDEN_COUNT} golden fixtures matched (see above)"
    fi
  fi
fi

# ─── 4. CloudWatch [PMTAWaveScheduler] first-wave fire ───────────────────────
note "Step 4 · CloudWatch ${LOG_GROUP} grep [PMTAWaveScheduler]"
if ! command -v aws >/dev/null 2>&1; then
  err "aws CLI not installed; skipping CloudWatch check"
else
  SINCE_MS=$(($(date -u +%s) * 1000 - 30 * 60 * 1000))
  EVENTS=$(aws logs filter-log-events \
    --region "${AWS_REGION}" \
    --log-group-name "${LOG_GROUP}" \
    --start-time "${SINCE_MS}" \
    --filter-pattern '"[PMTAWaveScheduler]"' \
    --max-items 50 \
    --query 'events[].message' \
    --output text 2>/dev/null || true)
  if [ -z "${EVENTS}" ] || [ "${EVENTS}" = "None" ]; then
    err "No [PMTAWaveScheduler] log lines in last 30m — wave dispatcher may be starved"
  else
    EVENT_COUNT=$(printf '%s\n' "${EVENTS}" | wc -l | tr -d ' ')
    ok "Found ${EVENT_COUNT} [PMTAWaveScheduler] events in last 30m"
  fi
fi

# ─── Summary ─────────────────────────────────────────────────────────────────
printf '\n=========================================\n'
printf 'Send-Day Planner First-Deploy Verification\n'
printf 'PASS=%d FAIL=%d\n' "${PASS}" "${FAIL}"
printf '=========================================\n'
for line in "${REPORT[@]}"; do
  printf '  %s\n' "${line}"
done
printf '\n'

if [ "${FAIL}" -gt 0 ]; then
  printf 'OVERALL: FAIL — DO NOT mark deploy complete. Investigate above.\n'
  exit 1
fi
printf 'OVERALL: PASS — Send-Day Planner deploy verified end-to-end.\n'
exit 0
