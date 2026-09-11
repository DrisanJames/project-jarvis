package main

// Content Desk schema (2026-09-11). The editorial system that drafts brand-site
// articles under full human review (internal/contentdesk). Appended to the
// runStartupMigrations list after contractFulfillmentMigrations, exactly like
// the REQ-118 slices: a package-level value a unit test can parse without a
// database (content_desk_migrations_test.go).
//
// Every entry is ONE statement (migrationSkipProbe classifies by leading
// keyword — a trailing second statement would silently never land once the
// first exists) and O(1) inside the 5s budget: the tables are new and empty,
// the seed is 27 rows, and the indexes are built on empty tables. Nothing here
// touches a send-path table. Rollback = DROP the content_* tables; no other
// table references them.

var contentDeskMigrations = []struct {
	name string
	sql  string
}{
	{"content_desk_sites", `CREATE TABLE IF NOT EXISTS content_sites (
		id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		org_id      UUID NOT NULL,
		brand_code  TEXT NOT NULL,
		domain      TEXT NOT NULL,
		surface     TEXT NOT NULL CHECK (surface IN ('static', 'mongo', 'qf')),
		enabled     BOOLEAN NOT NULL DEFAULT TRUE,
		voice       JSONB NOT NULL DEFAULT '{}'::jsonb,
		categories  JSONB NOT NULL DEFAULT '[]'::jsonb,
		adapter     JSONB NOT NULL DEFAULT '{}'::jsonb,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE (org_id, domain)
	)`},
	// 27 properties. Codes + apexes from agents/registry/brand_metadata.py;
	// the 23 static sites are exactly the domains in
	// review-forge/scripts/deploy-static-blog-feeds.sh; mongo = HT/MH/DB;
	// quizfiesta is the qf surface, seeded disabled. Seeded for the
	// single-tenant org (the seed_gmail_bans precedent).
	{"content_desk_seed_sites", `INSERT INTO content_sites (org_id, brand_code, domain, surface, enabled)
		SELECT '00000000-0000-0000-0000-000000000001'::uuid, v.code, v.domain, v.surface, v.enabled
		FROM (VALUES
			('DB',  'discountblog.com',           'mongo',  TRUE),
			('HT',  'historythinking.com',        'mongo',  TRUE),
			('MH',  'myownhealth.net',            'mongo',  TRUE),
			('QF',  'quizfiesta.com',             'qf',     FALSE),
			('BW',  'businessweeklypro.com',      'static', TRUE),
			('FC',  'financialcalculate.com',     'static', TRUE),
			('CP',  'consumerpro.net',            'static', TRUE),
			('HW',  'homewarrantyservices.org',   'static', TRUE),
			('RR',  'refinanceratesusa.com',      'static', TRUE),
			('TT',  'thingoftheday.org',          'static', TRUE),
			('YI',  'yourinsurancehub.com',       'static', TRUE),
			('MR',  'myrepairdiy.com',            'static', TRUE),
			('CI',  'casainsure.com',             'static', TRUE),
			('LP',  'learnpersonalloans.com',     'static', TRUE),
			('RB',  'ratesbazar.com',             'static', TRUE),
			('WF',  'warrantyforyou.com',         'static', TRUE),
			('MP',  'mypersonalfinancial.com',    'static', TRUE),
			('PD',  'paymydebit.com',             'static', TRUE),
			('TR',  'theretirementblog.com',      'static', TRUE),
			('BCC', 'bestcreditcare.com',         'static', TRUE),
			('USF', 'us-finance.com',             'static', TRUE),
			('YFB', 'yourfinancialblog.com',      'static', TRUE),
			('HLJ', 'homeloansbyjaime.com',       'static', TRUE),
			('FTH', 'firsttimebuyerhomeloan.com', 'static', TRUE),
			('HTM', 'hometracmortgage.com',       'static', TRUE),
			('AAD', 'aadwd.com',                  'static', TRUE),
			('HFC', 'hfcl.net',                   'static', TRUE)
		) AS v(code, domain, surface, enabled)
		ON CONFLICT (org_id, domain) DO NOTHING`},
	{"content_desk_briefs", `CREATE TABLE IF NOT EXISTS content_briefs (
		id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		org_id          UUID NOT NULL,
		site_id         UUID NOT NULL REFERENCES content_sites(id),
		format          TEXT NOT NULL,
		reader_question TEXT NOT NULL,
		angle           TEXT NOT NULL DEFAULT '',
		outline         JSONB NOT NULL DEFAULT '[]'::jsonb,
		category        TEXT NOT NULL,
		consequential   BOOLEAN NOT NULL DEFAULT FALSE,
		status          TEXT NOT NULL DEFAULT 'open',
		created_by      TEXT NOT NULL DEFAULT '',
		created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`},
	// APPEND-ONLY: (claim_id, version) is the identity; a correction is a new
	// version, enforced by the no-update trigger below.
	{"content_desk_claims", `CREATE TABLE IF NOT EXISTS content_claims (
		claim_id          UUID NOT NULL,
		version           INT NOT NULL CHECK (version >= 1),
		org_id            UUID NOT NULL,
		origin_article_id UUID,
		claim_key         TEXT NOT NULL DEFAULT '',
		question          TEXT NOT NULL DEFAULT '',
		answer            TEXT NOT NULL DEFAULT '',
		text              TEXT NOT NULL,
		type              TEXT NOT NULL CHECK (type IN ('sourced_fact', 'calculation', 'assumption', 'interpretation')),
		source_url        TEXT NOT NULL DEFAULT '',
		passage           TEXT NOT NULL DEFAULT '',
		context           TEXT NOT NULL DEFAULT '',
		published_at      TEXT NOT NULL DEFAULT '',
		effective_at      TEXT NOT NULL DEFAULT '',
		retrieved_at      TIMESTAMPTZ,
		jurisdiction      TEXT NOT NULL DEFAULT '',
		population        TEXT NOT NULL DEFAULT '',
		conditions        TEXT NOT NULL DEFAULT '',
		status            TEXT NOT NULL CHECK (status IN ('supported', 'insufficient_evidence', 'conflicting')),
		derivation        TEXT NOT NULL CHECK (derivation IN ('primary', 'rederive')),
		calc              JSONB,
		created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (claim_id, version)
	)`},
	{"content_desk_claims_append_only_fn", `CREATE OR REPLACE FUNCTION content_claims_append_only() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'content_claims is append-only: write a new version instead of %', TG_OP;
		END
		$$`},
	{"content_desk_claims_append_only_trigger", `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'content_claims_no_update_delete') THEN
			CREATE TRIGGER content_claims_no_update_delete BEFORE UPDATE OR DELETE ON content_claims
				FOR EACH ROW EXECUTE FUNCTION content_claims_append_only();
		END IF;
	END $$`},
	{"content_desk_articles", `CREATE TABLE IF NOT EXISTS content_articles (
		id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		org_id              UUID NOT NULL,
		site_id             UUID NOT NULL REFERENCES content_sites(id),
		brief_id            UUID REFERENCES content_briefs(id),
		slug                TEXT NOT NULL,
		status              TEXT NOT NULL DEFAULT 'drafting' CHECK (status IN ('drafting', 'in_review', 'changes_requested',
		                        'approved', 'publishing', 'live_confirmed', 'withdrawn', 'rejected')),
		current_revision_id UUID,
		harvestable         BOOLEAN NOT NULL DEFAULT FALSE,
		run_requested_at    TIMESTAMPTZ,
		withdrawn_at        TIMESTAMPTZ,
		created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE (site_id, slug)
	)`},
	{"content_desk_revisions", `CREATE TABLE IF NOT EXISTS content_revisions (
		id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		org_id        UUID NOT NULL,
		article_id    UUID NOT NULL REFERENCES content_articles(id),
		revision_hash TEXT NOT NULL,
		package       JSONB NOT NULL,
		claim_refs    JSONB NOT NULL DEFAULT '[]'::jsonb,
		checks        JSONB NOT NULL DEFAULT '{}'::jsonb,
		usage         JSONB NOT NULL DEFAULT '{}'::jsonb,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE (article_id, revision_hash)
	)`},
	{"content_desk_reviews", `CREATE TABLE IF NOT EXISTS content_reviews (
		id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		org_id        UUID NOT NULL,
		revision_id   UUID NOT NULL REFERENCES content_revisions(id),
		revision_hash TEXT NOT NULL,
		reviewer      TEXT NOT NULL,
		role          TEXT NOT NULL CHECK (role IN ('primary', 'second')),
		decision      TEXT NOT NULL CHECK (decision IN ('approve', 'reject', 'changes')),
		findings      JSONB NOT NULL DEFAULT '[]'::jsonb,
		accepted_ids  JSONB NOT NULL DEFAULT '[]'::jsonb,
		minutes       NUMERIC(8,2),
		created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`},
	{"content_desk_releases", `CREATE TABLE IF NOT EXISTS content_releases (
		id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		org_id        UUID NOT NULL,
		site_id       UUID NOT NULL REFERENCES content_sites(id),
		manifest      JSONB NOT NULL DEFAULT '[]'::jsonb,
		manifest_hash TEXT NOT NULL,
		status        TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'building', 'deployed', 'live_confirmed', 'failed')),
		error         TEXT,
		started_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		finished_at   TIMESTAMPTZ
	)`},
	// DB-level half of the per-site release lock: at most one in-flight
	// release per site, whatever the application does.
	{"content_desk_releases_inflight_uq", `CREATE UNIQUE INDEX IF NOT EXISTS uq_content_releases_inflight
		ON content_releases (site_id) WHERE status IN ('pending', 'building', 'deployed')`},
	{"content_desk_pipeline_runs", `CREATE TABLE IF NOT EXISTS content_pipeline_runs (
		id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		org_id      UUID NOT NULL,
		article_id  UUID NOT NULL REFERENCES content_articles(id),
		stage       TEXT NOT NULL,
		input_hash  TEXT NOT NULL,
		status      TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
		attempts    INT NOT NULL DEFAULT 0,
		output      JSONB,
		error       TEXT,
		started_at  TIMESTAMPTZ,
		finished_at TIMESTAMPTZ,
		UNIQUE (article_id, stage, input_hash)
	)`},
	{"content_desk_eval_cases", `CREATE TABLE IF NOT EXISTS content_eval_cases (
		id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		org_id     UUID NOT NULL,
		suite      TEXT NOT NULL CHECK (suite IN ('regression', 'qualification', 'development')),
		category   TEXT NOT NULL DEFAULT '',
		input      JSONB NOT NULL DEFAULT '{}'::jsonb,
		expected   JSONB NOT NULL DEFAULT '{}'::jsonb,
		created_by TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`},
	{"content_desk_eval_runs", `CREATE TABLE IF NOT EXISTS content_eval_runs (
		id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		org_id                     UUID NOT NULL,
		case_id                    UUID NOT NULL REFERENCES content_eval_cases(id),
		revision_or_prompt_version TEXT NOT NULL,
		passed                     BOOLEAN NOT NULL,
		details                    JSONB NOT NULL DEFAULT '{}'::jsonb,
		created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`},
	{"content_desk_usage_daily", `CREATE TABLE IF NOT EXISTS content_usage_daily (
		org_id     UUID NOT NULL,
		day        DATE NOT NULL,
		usd        NUMERIC(12,6) NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (org_id, day)
	)`},
	// Worker queue scan + review-queue/ops reads. Built on empty tables.
	{"content_desk_articles_queue_idx", `CREATE INDEX IF NOT EXISTS idx_content_articles_queue
		ON content_articles (run_requested_at) WHERE run_requested_at IS NOT NULL`},
	{"content_desk_articles_org_status_idx", `CREATE INDEX IF NOT EXISTS idx_content_articles_org_status
		ON content_articles (org_id, status, updated_at)`},
	{"content_desk_claims_org_idx", `CREATE INDEX IF NOT EXISTS idx_content_claims_org_claim
		ON content_claims (org_id, claim_id, version)`},
	{"content_desk_reviews_revision_idx", `CREATE INDEX IF NOT EXISTS idx_content_reviews_revision
		ON content_reviews (revision_id, created_at)`},
	{"content_desk_runs_org_status_idx", `CREATE INDEX IF NOT EXISTS idx_content_pipeline_runs_org_status
		ON content_pipeline_runs (org_id, status, finished_at)`},
	{"content_desk_releases_site_idx", `CREATE INDEX IF NOT EXISTS idx_content_releases_site
		ON content_releases (org_id, site_id, started_at)`},
}
