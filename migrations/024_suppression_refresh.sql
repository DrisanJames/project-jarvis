-- Migration 024: Automated Suppression Refresh Engine
-- Provides daily refresh of advertiser suppression lists with full audit trail
-- Operates within 12PM-12AM MST window for campaign safety

-- =============================================================================
-- TABLE 1: suppression_refresh_sources
-- Master registry of every advertiser suppression URL mapped to offers
-- =============================================================================
CREATE TABLE IF NOT EXISTS suppression_refresh_sources (
    id                  VARCHAR(100) PRIMARY KEY,
    offer_id            VARCHAR(50),
    campaign_name       VARCHAR(500),
    suppression_url     TEXT,
    source_provider     VARCHAR(50) DEFAULT 'unknown',
    ga_suppression_id   VARCHAR(50),
    internal_list_id    VARCHAR(100),
    is_active           BOOLEAN DEFAULT FALSE,
    refresh_group       VARCHAR(100),
    priority            INT DEFAULT 100,
    last_refreshed_at   TIMESTAMP WITH TIME ZONE,
    last_refresh_status VARCHAR(20),
    last_entry_count    INT DEFAULT 0,
    last_refresh_ms     INT,
    last_error          TEXT,
    notes               TEXT,
    created_at          TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at          TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_refresh_sources_active ON suppression_refresh_sources(is_active);
CREATE INDEX IF NOT EXISTS idx_refresh_sources_group ON suppression_refresh_sources(refresh_group);
CREATE INDEX IF NOT EXISTS idx_refresh_sources_offer ON suppression_refresh_sources(offer_id);
CREATE INDEX IF NOT EXISTS idx_refresh_sources_provider ON suppression_refresh_sources(source_provider);
CREATE INDEX IF NOT EXISTS idx_refresh_sources_internal_list ON suppression_refresh_sources(internal_list_id);

-- =============================================================================
-- TABLE 2: suppression_refresh_cycles
-- One row per daily refresh run - tracks overall progress
-- =============================================================================
CREATE TABLE IF NOT EXISTS suppression_refresh_cycles (
    id                      VARCHAR(100) PRIMARY KEY,
    started_at              TIMESTAMP WITH TIME ZONE NOT NULL,
    completed_at            TIMESTAMP WITH TIME ZONE,
    status                  VARCHAR(20) NOT NULL DEFAULT 'running',
    total_sources           INT DEFAULT 0,
    completed_sources       INT DEFAULT 0,
    failed_sources          INT DEFAULT 0,
    skipped_sources         INT DEFAULT 0,
    total_entries_downloaded BIGINT DEFAULT 0,
    total_new_entries       BIGINT DEFAULT 0,
    avg_download_ms         INT,
    triggered_by            VARCHAR(50) DEFAULT 'scheduler',
    resumed_from_source     VARCHAR(100),
    error_message           TEXT
);

CREATE INDEX IF NOT EXISTS idx_refresh_cycles_status ON suppression_refresh_cycles(status);
CREATE INDEX IF NOT EXISTS idx_refresh_cycles_started ON suppression_refresh_cycles(started_at DESC);

-- =============================================================================
-- TABLE 3: suppression_refresh_logs
-- Per-source outcome within a cycle - full audit trail
-- =============================================================================
CREATE TABLE IF NOT EXISTS suppression_refresh_logs (
    id                  VARCHAR(100) PRIMARY KEY,
    cycle_id            VARCHAR(100) NOT NULL,
    source_id           VARCHAR(100) NOT NULL,
    started_at          TIMESTAMP WITH TIME ZONE NOT NULL,
    completed_at        TIMESTAMP WITH TIME ZONE,
    status              VARCHAR(20) NOT NULL DEFAULT 'pending',
    entries_downloaded  INT DEFAULT 0,
    entries_new         INT DEFAULT 0,
    entries_unchanged   INT DEFAULT 0,
    file_size_bytes     BIGINT DEFAULT 0,
    download_ms         INT,
    processing_ms       INT,
    http_status_code    INT,
    content_type        VARCHAR(100),
    error_message       TEXT,
    retry_count         INT DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_refresh_logs_cycle ON suppression_refresh_logs(cycle_id);
CREATE INDEX IF NOT EXISTS idx_refresh_logs_source ON suppression_refresh_logs(source_id);
CREATE INDEX IF NOT EXISTS idx_refresh_logs_status ON suppression_refresh_logs(status);

-- =============================================================================
-- TABLE 4: suppression_refresh_groups
-- Named groups for organizing which sources to refresh
-- =============================================================================
CREATE TABLE IF NOT EXISTS suppression_refresh_groups (
    name                VARCHAR(100) PRIMARY KEY,
    description         TEXT,
    is_active           BOOLEAN DEFAULT TRUE,
    source_count        INT DEFAULT 0,
    created_at          TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at          TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);
