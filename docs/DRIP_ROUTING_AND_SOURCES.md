# Drip routing & analytics sources — SES / PMTA / kumo / app, disambiguated

**Written 2026-08-22 (drip revamp legacy audit). Every claim carries file:line, verified that day.**

The words "ses", "pmta", "kumo" appear in TWO independent vocabularies — transport routing and
analytics sources — and `app` belongs to only one of them. Conflating the two vocabularies is the
single most common misreading of drip numbers.

## The answer to "does APP mean PowerMTA?"

**No. `app` is never a transport.** A repo-wide search for `transport|route|routing_mode = 'app'`
returns zero hits. `source='app'` in the analytics lake (`ignite_analytics.email_events`) is the
**PG→lake mirror of our own first-party tracking layer** (`mailing_tracking_events` — the open
pixel / click redirect this platform serves), pushed by `agents/jobs/pg_to_lake.py`. PowerMTA's
lake label is `source='pmta'`, produced by the PMTA accounting-record ingestor
(`internal/engine/ingest.go:385-415`).

## 1. Transport vocabulary (how mail leaves)

Transport is decided **per sending profile**, split across three fields of
`mailing_sending_profiles` — this split is the core naming defect:

| Field | Values | Meaning |
|---|---|---|
| `vendor_type` | `pmta` (also legacy `ses`,`sparkpost`,`mailgun`,`sendgrid`) | Which sender implementation. All live profiles are `pmta`. |
| `routing_mode` | `''` or `'kumo'` (only two valid values — `sending_profiles_handlers.go:975-980`) | `'kumo'` → KumoMTA injector (`esp_profile.go:150-162`); else PMTA HTTP bridge. **Cannot express SES.** |
| `via_ses` | bool | Not a router. Affects headers (`X-SES-*`, `send_worker.go:1949-1966`) and suppresses first-party pixel/click injection (`:1972-1978`). The actual SES relay is a **PMTA config concern**: the profile's `<prefix>-ses-pool` VMTA relays to `email-smtp.us-west-1.amazonaws.com:587`. |

So: **"SES-routed" mail is still injected into PMTA** (or sent via an SES tenant profile) — PMTA
relays it out through SES. The lake shows this as `pmta` rows with `relayed_to_ses` plus `ses`
rows from the SNS webhook.

The send worker's ESP switch (`send_worker.go:2157-2172`) is **inert in production** — all five
slots are the same `ProfileBasedSender` (`cmd/server/main.go:574-576`), which re-reads
`vendor_type` itself in `esp_profile.go`.

### How a PARTNER-DRIP send picks its transport (decided at deploy time, not send time)

`partitionWaveBySESProfile` (`partner_drip_orchestrator.go:1258`):

1. `PARTNER_DRIP_ROUTE_ALL_SES` (default **ON**, `:1235`) → the wave defaults to the brand's SES
   tenant profile (`dripBrandSESProfiles()` `:1208-1230`, 16 hardcoded brand→profile UUIDs).
2. An explicit `(brand, ISP)` pin from `dripBrandISPSESProfiles()` (`:1176-1199`, default
   `ht×microsoft`; env `PARTNER_DRIP_BRAND_ISP_SES_PROFILES` REPLACES the default) overrides.
3. Governed/kumo brands (mpf pmd trb bcc usf yfb hlj fth htm) are absent from the SES map →
   empty profileID → by-domain profile resolution → their profile carries `routing_mode='kumo'`
   → KumoMTA. (`routeLabel` historically logged these as "pmta"; fixed in the 2026-08-22 revamp.)
4. SES-pinned groups get sending domain rewritten `em.<apex>` → `m.<apex>` (`:1315-1321`) and the
   campaign name suffixed `[ses:<8-char>]` (`:1325`).

Gmail→SES is **board doctrine, not drip code** — in the drip, gmail is simply held at cap 0
(`DefaultPerISPCapPerWave`/`DefaultNewRecordDailyISPCaps`). `applyISPBrandRouting` /
`applyFollowupGmailRouting` are **cap gates (brand eligibility), not transport routing**, despite
their names.

## 2. Analytics-source vocabulary (where a lake row came from)

| `email_events.source` | Producer | Carries | Missing |
|---|---|---|---|
| `ses` | SES SNS webhook (`handlers_ses_events.go:577-600`) | attempted, delivered, open, click, bounce, complaint | — |
| `pmta` | PMTA accounting ingest (`engine/ingest.go`) | delivery_delay, `relayed_to_ses`, bounces | direct `delivered` ~nil (it relays) |
| `kumo` | Kumo accounting ingest | delivered, bounces, delays | **no sent/open/click** |
| `app` | `pg_to_lake.py` mirror of `mailing_tracking_events` | opened/clicked/unsubscribed same-day (+ nightly prior-day `delivered` batch) | same-day delivered/bounced are 0 **by construction** |

- `app` opens/clicks SHADOW `ses` (~78% open / ~94% click overlap on drip lanes) — **never add
  app + ses**. `app` is the only engagement view for pmta-/kumo-routed mail.
- `route_type` values: `pmta_direct`, `kumo_direct`, `ses`, `ses_tenant`, `ses_direct`, `''`
  (app rows). `ses_tenant` is stamped when `IsPMTARelayedToSES` fires — a substring heuristic on
  pool/VMTA names containing "ses" (`engine/ses_relay.go:22-25`); never name a non-SES pool with
  "ses" in it.
- `mailing_message_log.esp_type` is `'pmta'` even for SES-routed drip sends (profiles are
  `vendor_type='pmta'`) — do not read it as transport truth.
- `mailing_tracking_events` has **no source column at all**; 'app' exists only as a lake label.

## 3. Reading rules (the ones that prevent wrong numbers)

1. Delivery truth per (lane × ISP) = **SES VDM / the lake**, never PG counters.
2. kumo delivery % = terminal outcomes only (delivered+hard+soft, exclude delivery_delay);
   kumo engagement comes from PG (`app` mirror), not the lake.
3. `via_ses` lanes are pixel-blind in PG (injection suppressed) — their engagement is in
   `source='ses'` (SES's own tracking), mirrored partially into `app`.
4. A drip campaign's transport is readable from its name (`[ses:...]` suffix = SES tenant;
   governed brand = kumo; otherwise PMTA bridge) and from the wave's routeLabel in logs.
