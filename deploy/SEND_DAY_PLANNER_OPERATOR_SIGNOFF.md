# Send-Day Planner — Operator Sign-Off Checklist (Phase 5)

This is the 13-item checklist the operator works through before declaring
the first canvas-driven send-day a success. Generated per the plan
(.cursor/plans/send-day_planner_canvas_*.plan.md, Phase 5) and the
testing-rule mandate that every change is verified end-to-end with
observed-with-eyes evidence — not "tests pass, ship it"
(.cursor/rules/testing.mdc §"The Golden Rule").

The companion automation script is
`upside-down/deploy/verify_send_day_planner.sh`. Items marked
[script] are automated by that script; everything else is a manual
human-in-the-loop gate.

## Pre-Deploy (BEFORE clicking the canvas Deploy button)

- [ ] **1. Six-gate strip is fully green.** Open the Send-Day Planner;
      every chip in the gate strip (A through F) shows `pass`. Gate E
      (audit reviewed) is the only operator-attested gate; click the
      tile to attest after eyeballing the audit JSON drawer below.

- [ ] **2. Audit JSON drawer dry-runs all 16 cells with HTTP 200.** Open
      the drawer, click "Run dry-run for all", verify every cell card
      reads `HTTP 200` and no card surfaces an INVARIANT FAIL line.

- [ ] **3. Volume reconciliation tile shows passing ramp math.** Today's
      planned ≥ yesterday's planned × 1.20 × 0.95. If short, roll back
      the matrix transformation that under-shot and re-attest Gate F.

- [ ] **4. Banned creatives check.** No grid cell carries the
      `BANNED CREATIVE` red badge. If a banned filename slipped into a
      cell, swap the family for that cell before deploying.

## Deploy

- [ ] **5. Click Deploy and confirm the count modal.** The confirmation
      window says `Deploy 16 campaigns to /api/mailing/pmta-campaign/deploy?
      This is LIVE`. Confirm only when the count matches what you
      planned (16 for mature_only milestone).

## Post-Deploy (FIRST 60 minutes — block here)

- [ ] **6. [script] /health git_sha matches deployed commit.** Run
      `verify_send_day_planner.sh` Step 1; the script's exit code is 0
      and the line `PASS · /health git_sha=… matches expected …` is
      printed.

- [ ] **7. [script] 16 campaigns visible in /api/mailing/campaigns?
      search=<DATE_PREFIX>.** Step 2 of the script. Anything other than
      16 means a partial deploy — investigate Toast errors in the
      browser before re-deploying anything.

- [ ] **8. [script] Field-by-field DB rows match canvas goldens.**
      Step 3 of the script reads each `*.post.json` fixture and
      diffs `name / sending_domain / target_isps / isp_quotas / scheduled_at /
      status` against the corresponding `mailing_campaigns` row.
      Any mismatch line is a contract violation.

- [ ] **9. Per-campaign `total_recipients > 0` after audience finalizes.**
      Within ~5 min each campaign should leave `finalizing_audience` and
      land in `sending` with `total_recipients > 0`. A campaign stuck at
      `finalizing_audience` past 10 min OR landing in `failed` with
      `total_recipients = 0` almost always means the source-field bug
      regressed (see .cursor/rules/sending-throttle.mdc Apr 28 incident)
      — quarantine the campaign and check `last_error`.

- [ ] **10. [script] First-wave fire visible in CloudWatch
      [PMTAWaveScheduler] log lines.** Step 4 of the script. Absent
      events in the 30-minute window means the wave dispatcher is
      starved (run the pre-deploy janitor query from
      .cursor/rules/sending-throttle.mdc Gate B).

## Post-Deploy (FIRST 24 hours — soak)

- [ ] **11. Per-brand mta delivery rates within historical envelope.**
      Open the existing analytics dashboards; for each brand the
      delivery rate over the send-window should sit within ±2 percentage
      points of the prior 7-day rolling mean. Outliers ≥ 5 pts trigger
      a deep-dive (likely creative or DKIM regression).

- [ ] **12. No spike in `[ConvictionEngine] PAUSE` events for the new
      campaigns.** Grep CloudWatch for the campaign IDs returned by
      Step 2; absence of pause events confirms the deploy didn't
      destabilize sender reputation.

- [ ] **13. Operator updates project memory + writes the post-deploy
      audit.** Add a `bug` or `event` entity to `project-memory` with
      observations describing what worked, what didn't, what surprised
      you. Commit the audit JSON exported from the drawer to
      `.scratch/<DATE>_mature_send_day_audit.json`.

## How to use

```bash
# Before deploy: human-eye gates 1–4 in the canvas.
# After deploy: run the automation script.
AWS_REGION=us-west-2 \
PUBLIC_BASE_URL=https://projectjarvis.io \
EXPECTED_GIT_SHA=$(git rev-parse HEAD) \
ORG_ID=00000000-0000-0000-0000-000000000001 \
ADMIN_KEY=<see config/config.yaml> \
DATE_PREFIX=05122026 \
PROD_DB_URL='postgres://…' \
./upside-down/deploy/verify_send_day_planner.sh
```

If the script exits non-zero OR any manual item is unchecked, the
deploy is NOT complete. Per testing.mdc anti-pattern #4, "the user
will tell me if it's broken" is forbidden — the operator is
responsible for catching their own failures here.
