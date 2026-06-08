# Rebrand — Infrastructure Identifiers (NOT changed in the code rebrand)

The 2026-06-08 rebrand (IGNITE → Project Jarvis, Ignite Media Group → James Ventures Corp,
ignitemediagroup.co → jamesventurescorp.com) scrubbed all **brand/identity** text in the
platform UI, code, logs, docs, and the email CAN-SPAM footer + OAuth domain.

The following are **load-bearing infrastructure identifiers** left intact on purpose — renaming
them is a separate, deliberate migration with a maintenance window, not a code change. None are
brand-visible. Tackle individually if/when desired.

| # | Identifier | Where | Why left / risk | Migration sketch |
|---|---|---|---|---|
| 1 | Go module `github.com/ignite/sparkpost-monitor` | `go.mod` + 199 files | Pure refactor, no prod effect, but huge import churn | `go mod edit -module …`; `gofmt -r`/sed all imports; rebuild |
| 2 | DB `ignite:ignite_secret@…/ignite` (user/name/pw) | `config.yaml`, ops scripts | **HIGH** — auth outage if mismatched | `ALTER ROLE/DATABASE … RENAME`; rotate pw; update DSN everywhere; window |
| 3 | AWS `ignite-service` / `ignite-upside-down` (ECS svc/task/ECR) / `ignite-server` (container) | `deploy/*.sh`, AWS | **HIGH** — deploy + traffic cutover | New ECS service/task-def/ECR; update `deploy.sh`; ALB target swap; delete old |
| 4 | S3 bucket `ignite-email-images-prod` | image CDN | Medium — image hosting (CDN domain already `img.projectjarvis.io`) | New bucket; copy objects; update code + image config |
| 5 | Snowflake `IGNITE_DATA_LAKE` / `ignitedevelopers` | `config.yaml` | External warehouse | Rename DB/user in Snowflake; update config |
| 6 | `ignite.media` fallback domains (`track.ignite.media`, `noreply@ignite.media`) | `journey_executor.go` fallbacks | Prod uses `TRACKING_URL`/sending-profile env, so fallbacks rarely fire | Stand up tracking/sending DNS on the new domain, then repoint fallbacks |
| 7 | `ignitemedia.com` → repointed to `jamesventurescorp.com` (the `IGN` internal/default brand) | `CampaignPurposeTab.tsx`, `everflow/types.go` | If `IGN` ever actually sends, the new domain needs DKIM/sending setup | Provision sending for jamesventurescorp.com or retire the `IGN` fallback |
| 8 | PMTA config marker `# … (managed by IGNITE)` | `handlers_pmta_campaign.go` 1718/1764 | Renaming orphans cleanup of existing on-server config blocks | Rename in code **and** one-time `sed` each PMTA server's `/etc/pmta/config` |
| 9 | `session_secret: "ignite-session-secret-…"` | `config.yaml` | Cosmetic string inside a secret | Rotate the session secret (invalidates active sessions) |

## Took effect via the code rebrand (deploy required)
- Email **CAN-SPAM footer** company → James Ventures Corp (address unchanged).
- **OAuth login allowlist** → jamesventurescorp.com.
- **Live org name** → James Ventures Corp via the idempotent `rebrand_org_to_james_ventures`
  entry in `runStartupMigrations()` (applies on next boot/deploy).
- All platform UI / logs / Slack alerts / docs → Project Jarvis.
- Dispatched-email content: IGNITE removed, **no** "Project Jarvis" injected (journey from-name
  fallback → "Notifications"; test-send strings neutralized).
