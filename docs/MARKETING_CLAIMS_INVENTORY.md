# Marketing claims inventory

Each capability the public site mentions, its real state, and the evidence. Copy must stay inside this
table: `unsupported` items do not appear on the site, `planned` items appear only as planned. Paths are
relative to `upside-down/`. Checked 2026-10-31 against the working tree.

| Capability | State | Evidence | What the site says |
|---|---|---|---|
| Create, schedule, send campaigns as drafts | live | `internal/api/campaign_builder.go:122-147`, `:136` (schedule), `internal/api/campaign_builder_send_async.go:115` | "Start every campaign as a draft" |
| Preview before sending | live | `internal/api/campaign_builder.go:122-147`, `internal/api/mailing_handlers_full.go:644` | "Preview the campaign before it sends" |
| Templates and folders | live | `internal/api/mailing_templates.go:40`, `internal/api/template_folder_handlers.go:108`, `:354` | "Start from a message, a template or a short brief" |
| Import contacts from a file | live | `internal/api/mailing_import.go:42` | "Import contacts from a file" |
| Import opt-outs | live | `internal/api/suppression_crud_handlers.go:431`, `internal/api/suppression_list_handlers.go:86` | FAQ, migration |
| Segments by fields and activity | live | `internal/api/mailing_segments.go:174`, `:1102` | "Group people by fields and activity" |
| Unsubscribes and exclusions honoured | live | `internal/api/suppression_crud_handlers.go:431`, `internal/preferences/hub.go:402` | "Keep unsubscribes and exclusions out of your sends" |
| Welcome and follow-up sequences | live | `web/src/components/mailing/components/JourneyBuilder.tsx`, `internal/worker/journey_executor.go` | Automation section |
| Campaign reporting | live | `internal/api/mailing_campaign_summary.go:158`, `:490`, `internal/api/campaign_builder_analytics.go:86`, `internal/api/server_routes_mailing.go:771` | Delivery, clicks, engagement. No conversion or revenue claim |
| AI subject lines | live | `internal/api/ai_content_handlers.go:75`, `internal/api/ai_subject_suggestions.go:118`, `internal/api/ai_subject_preheader_claude.go:224` | "Explore subject line options" |
| AI drafting | live | `internal/api/creative_studio.go`, `internal/api/creative_agent.go` | "Turn a short brief into a draft" |
| AI message review | live | `internal/api/ai_content_handlers.go:130` | "Review a message" |
| AI explanation of results | beta | `internal/api/marketing_agent*.go` (`/api/mailing/agent/chat`), `web/src/components/mailing/components/CampaignCopilot.tsx`. Built for the operator's data, not yet a customer feature | "Ask Jarvis to explain what you're looking at" |
| A/B testing | not verified | Not traced end to end | Not claimed |
| Google sign-in, approved accounts only | live | `internal/auth/auth.go` (`allowedIdentity`, `HandleCallback`) | `/trust` |
| Lead capture | live once deployed | `internal/api/marketing_leads.go` | Forms |
| Campaign approval workflow | unsupported | None found. Sending needs a person to act, but there is no approver step | Replaced by "Campaigns stay drafts until someone on your team schedules or sends them" |
| Per-permission limits on AI | unsupported | None found | Brief's "Set the boundaries" section omitted |
| Team roles and permissions | unsupported | `internal/api/org_context.go` scopes by organization only | Not claimed |
| Agency and client workspaces | unsupported | Same | Omitted |
| Generic conversion reporting | unsupported | Conversion data comes only from a single network feed (`internal/everflow/`) | Not claimed |
| Public API and API keys | planned | Session and admin key only. `/api/sites/v1/events` is the only external contract | "Isn't public yet", request form |
| Outbound webhooks | unsupported | Inbound only | Not claimed |
| Third-party integrations | planned | None for customers | Request form, no vendor names |
| Self-serve signup and billing | planned | None. New accounts complete a form and get no access | Pricing "Coming soon" |
| Migration of history or automations from another tool | unsupported | Import covers contacts and opt-outs only | Migration page says they do not move automatically |
| Certifications, compliance attestations | unsupported | None | Not claimed |
| Inbox placement, timing or undo guarantees | unsupported | Not offered | Not claimed |
