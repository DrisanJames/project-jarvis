-- Migration 025: PMTA IP Management Infrastructure
-- Adds tables for managing dedicated IPs, IP pools, PMTA servers,
-- DKIM keys, domain assignments, and IP warmup tracking.

-- IP address inventory
CREATE TABLE IF NOT EXISTS mailing_ip_addresses (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    ip_address INET NOT NULL,
    hostname VARCHAR(255) NOT NULL,
    -- Ownership tracking
    acquisition_type VARCHAR(20) DEFAULT 'purchased'
        CHECK (acquisition_type IN ('purchased','leased','provider','transferred')),
    broker VARCHAR(100),
    broker_reference VARCHAR(255),
    rir VARCHAR(10),
    cidr_block VARCHAR(20),
    asn VARCHAR(20),
    acquired_at TIMESTAMPTZ,
    -- Server assignment
    hosting_provider VARCHAR(50),
    pmta_server_id UUID,
    pool_id UUID,
    -- Status and health
    status VARCHAR(20) DEFAULT 'pending'
        CHECK (status IN ('pending','active','warmup','paused','blacklisted','retired')),
    warmup_stage VARCHAR(20) DEFAULT 'cold',
    warmup_day INTEGER DEFAULT 0,
    warmup_daily_limit INTEGER DEFAULT 50,
    warmup_started_at TIMESTAMPTZ,
    reputation_score DECIMAL(5,2) DEFAULT 0.0,
    rdns_verified BOOLEAN DEFAULT FALSE,
    rdns_last_checked TIMESTAMPTZ,
    -- Blacklist tracking
    blacklisted_on JSONB DEFAULT '[]',
    last_blacklist_check TIMESTAMPTZ,
    -- Lifetime counters
    total_sent BIGINT DEFAULT 0,
    total_delivered BIGINT DEFAULT 0,
    total_bounced BIGINT DEFAULT 0,
    total_complained BIGINT DEFAULT 0,
    last_sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(ip_address)
);

-- IP pools group IPs for rotation
CREATE TABLE IF NOT EXISTS mailing_ip_pools (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    name VARCHAR(255) NOT NULL,
    description TEXT,
    pool_type VARCHAR(20) DEFAULT 'dedicated'
        CHECK (pool_type IN ('dedicated','shared','warmup')),
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(organization_id, name)
);

-- Add FK from ip_addresses to pools (deferred to avoid ordering issues)
ALTER TABLE mailing_ip_addresses
    ADD CONSTRAINT fk_ip_addresses_pool
    FOREIGN KEY (pool_id) REFERENCES mailing_ip_pools(id) ON DELETE SET NULL;

-- PMTA server registry
CREATE TABLE IF NOT EXISTS mailing_pmta_servers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    name VARCHAR(255) NOT NULL,
    host VARCHAR(255) NOT NULL,
    smtp_port INTEGER DEFAULT 25,
    mgmt_port INTEGER DEFAULT 19000,
    mgmt_api_key TEXT,
    ssh_key_path TEXT,
    provider VARCHAR(50),
    status VARCHAR(20) DEFAULT 'active',
    last_health_check TIMESTAMPTZ,
    health_status VARCHAR(20) DEFAULT 'unknown',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Add FK from ip_addresses to pmta_servers
ALTER TABLE mailing_ip_addresses
    ADD CONSTRAINT fk_ip_addresses_pmta_server
    FOREIGN KEY (pmta_server_id) REFERENCES mailing_pmta_servers(id) ON DELETE SET NULL;

-- DKIM keys per domain (stored in platform, pushed to PMTA)
CREATE TABLE IF NOT EXISTS mailing_dkim_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    domain VARCHAR(255) NOT NULL,
    selector VARCHAR(63) NOT NULL,
    private_key_encrypted BYTEA NOT NULL,
    public_key TEXT NOT NULL,
    algorithm VARCHAR(20) DEFAULT 'rsa-sha256',
    key_size INTEGER DEFAULT 2048,
    dns_record_value TEXT,
    dns_verified BOOLEAN DEFAULT FALSE,
    active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(domain, selector)
);

-- IP-to-domain assignment (which IPs send for which domains)
CREATE TABLE IF NOT EXISTS mailing_ip_domain_assignments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ip_id UUID NOT NULL REFERENCES mailing_ip_addresses(id) ON DELETE CASCADE,
    domain_id UUID NOT NULL REFERENCES mailing_sending_domains(id) ON DELETE CASCADE,
    is_primary BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(ip_id, domain_id)
);

-- IP warmup schedule tracking
CREATE TABLE IF NOT EXISTS mailing_ip_warmup_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ip_id UUID NOT NULL REFERENCES mailing_ip_addresses(id) ON DELETE CASCADE,
    date DATE NOT NULL,
    planned_volume INTEGER NOT NULL,
    actual_sent INTEGER DEFAULT 0,
    actual_delivered INTEGER DEFAULT 0,
    actual_bounced INTEGER DEFAULT 0,
    actual_complained INTEGER DEFAULT 0,
    bounce_rate DECIMAL(5,4),
    complaint_rate DECIMAL(5,4),
    warmup_day INTEGER NOT NULL,
    status VARCHAR(20) DEFAULT 'pending'
        CHECK (status IN ('pending','in_progress','completed','paused','failed')),
    notes TEXT,
    UNIQUE(ip_id, date)
);

-- Update delivery servers to support PMTA type
ALTER TABLE mailing_delivery_servers
    DROP CONSTRAINT IF EXISTS mailing_delivery_servers_server_type_check;
ALTER TABLE mailing_delivery_servers
    ADD CONSTRAINT mailing_delivery_servers_server_type_check
    CHECK (server_type IN ('sparkpost','ses','smtp','mailgun','pmta'));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_ip_addresses_org ON mailing_ip_addresses(organization_id);
CREATE INDEX IF NOT EXISTS idx_ip_addresses_pool ON mailing_ip_addresses(pool_id);
CREATE INDEX IF NOT EXISTS idx_ip_addresses_status ON mailing_ip_addresses(status);
CREATE INDEX IF NOT EXISTS idx_ip_addresses_server ON mailing_ip_addresses(pmta_server_id);
CREATE INDEX IF NOT EXISTS idx_ip_pools_org ON mailing_ip_pools(organization_id);
CREATE INDEX IF NOT EXISTS idx_pmta_servers_org ON mailing_pmta_servers(organization_id);
CREATE INDEX IF NOT EXISTS idx_dkim_keys_domain ON mailing_dkim_keys(domain);
CREATE INDEX IF NOT EXISTS idx_dkim_keys_org ON mailing_dkim_keys(organization_id);
CREATE INDEX IF NOT EXISTS idx_ip_domain_assignments_ip ON mailing_ip_domain_assignments(ip_id);
CREATE INDEX IF NOT EXISTS idx_ip_domain_assignments_domain ON mailing_ip_domain_assignments(domain_id);
CREATE INDEX IF NOT EXISTS idx_warmup_log_ip_date ON mailing_ip_warmup_log(ip_id, date);
CREATE INDEX IF NOT EXISTS idx_warmup_log_status ON mailing_ip_warmup_log(status);
