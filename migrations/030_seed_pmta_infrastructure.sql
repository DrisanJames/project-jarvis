-- Migration 030: Seed PMTA Infrastructure
-- Populates the IP addresses, pools, sending profiles, and domains
-- for the OVH PMTA server (15.204.101.125) with 16 dedicated IPs.

DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    default_pool_id UUID;
    warmup_pool_id UUID;
    pmta_server_id UUID;
    profile_id UUID;
BEGIN
    -- 1. Create PMTA server record
    INSERT INTO mailing_pmta_servers (organization_id, name, host, smtp_port, mgmt_port, ssh_key_path, provider, status)
    VALUES (org_id, 'OVH PMTA 1', '15.204.101.125', 25, 19000, '~/.ssh/ovh_pmta', 'OVH', 'active')
    ON CONFLICT DO NOTHING
    RETURNING id INTO pmta_server_id;

    IF pmta_server_id IS NULL THEN
        SELECT id INTO pmta_server_id FROM mailing_pmta_servers WHERE organization_id = org_id AND name = 'OVH PMTA 1';
    END IF;

    -- 2. Create IP pools
    INSERT INTO mailing_ip_pools (organization_id, name, description, pool_type, status)
    VALUES (org_id, 'default-pool', 'All 16 IPs — production sending', 'dedicated', 'active')
    ON CONFLICT (organization_id, name) DO NOTHING
    RETURNING id INTO default_pool_id;

    IF default_pool_id IS NULL THEN
        SELECT id INTO default_pool_id FROM mailing_ip_pools WHERE organization_id = org_id AND name = 'default-pool';
    END IF;

    INSERT INTO mailing_ip_pools (organization_id, name, description, pool_type, status)
    VALUES (org_id, 'warmup-pool', 'First 4 IPs — IP warmup rotation', 'warmup', 'active')
    ON CONFLICT (organization_id, name) DO NOTHING
    RETURNING id INTO warmup_pool_id;

    IF warmup_pool_id IS NULL THEN
        SELECT id INTO warmup_pool_id FROM mailing_ip_pools WHERE organization_id = org_id AND name = 'warmup-pool';
    END IF;

    -- 3. Register all 16 IPs
    INSERT INTO mailing_ip_addresses (organization_id, ip_address, hostname, pool_id, pmta_server_id,
        acquisition_type, hosting_provider, cidr_block, status, warmup_stage, warmup_day, warmup_daily_limit,
        rdns_verified, reputation_score)
    VALUES
        (org_id, '15.204.22.176', 'mta1.mail.projectjarvis.io',  warmup_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'warming', 1, 200, true, 50.0),
        (org_id, '15.204.22.177', 'mta2.mail.projectjarvis.io',  warmup_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'warming', 1, 200, true, 50.0),
        (org_id, '15.204.22.178', 'mta3.mail.projectjarvis.io',  warmup_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'warming', 1, 200, true, 50.0),
        (org_id, '15.204.22.179', 'mta4.mail.projectjarvis.io',  warmup_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'warming', 1, 200, true, 50.0),
        (org_id, '15.204.22.180', 'mta5.mail.projectjarvis.io',  default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.181', 'mta6.mail.projectjarvis.io',  default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.182', 'mta7.mail.projectjarvis.io',  default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.183', 'mta8.mail.projectjarvis.io',  default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.184', 'mta9.mail.projectjarvis.io',  default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.185', 'mta10.mail.projectjarvis.io', default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.186', 'mta11.mail.projectjarvis.io', default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.187', 'mta12.mail.projectjarvis.io', default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.188', 'mta13.mail.projectjarvis.io', default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.189', 'mta14.mail.projectjarvis.io', default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.190', 'mta15.mail.projectjarvis.io', default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0),
        (org_id, '15.204.22.191', 'mta16.mail.projectjarvis.io', default_pool_id, pmta_server_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'cold', 0, 50, true, 50.0)
    ON CONFLICT DO NOTHING;

    -- 4. Create PMTA sending profile (routes through PMTA SMTP)
    INSERT INTO mailing_sending_profiles (organization_id, name, description, vendor_type,
        from_name, from_email, reply_email,
        smtp_host, smtp_port, smtp_username, smtp_password,
        sending_domain, tracking_domain, ip_pool,
        hourly_limit, daily_limit, status)
    VALUES (org_id, 'PMTA Warmup', 'ProjectJarvis.io via OVH PMTA (warmup phase)', 'pmta',
        'Ignite', 'hello@projectjarvis.io', 'reply@projectjarvis.io',
        '15.204.101.125', 25, 'ignite', 'xK9#mPtA2026!ovh',
        'projectjarvis.io', 'projectjarvis.io', 'warmup-pool',
        3200, 25600, 'active')
    ON CONFLICT DO NOTHING
    RETURNING id INTO profile_id;

    RAISE NOTICE 'Seeded PMTA infrastructure: pools=2, ips=16, profile=%', COALESCE(profile_id::text, 'exists');

    -- 5. Seed sending domains
    INSERT INTO mailing_domains (organization_id, domain, dkim_selector, dkim_key_path,
        spf_status, dkim_status, dmarc_status, tracking_domain, is_active, warmup_stage, daily_limit)
    VALUES (org_id, 'projectjarvis.io', 'pj1', '/etc/pmta/dkim/projectjarvis.io.key',
        'verified', 'verified', 'verified', 'projectjarvis.io', true, 1, 25600)
    ON CONFLICT DO NOTHING;

END $$;
