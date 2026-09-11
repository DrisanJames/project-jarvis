package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var contentDeskTables = []string{
	"content_sites", "content_briefs", "content_claims", "content_articles", "content_revisions", "content_reviews",
	"content_releases", "content_pipeline_runs", "content_eval_cases", "content_eval_runs", "content_usage_daily",
	"content_ops_reports",
}

func TestContentDeskMigrations_GatesAndWiring(t *testing.T) {
	existing := map[string]bool{}
	for _, m := range criticalSendPathDDL {
		existing[m.name] = true
	}
	for _, m := range dripSupplyMigrations {
		existing[m.name] = true
	}
	for _, m := range contractFulfillmentMigrations {
		existing[m.name] = true
	}
	seen := map[string]bool{}
	created := map[string]bool{}
	createRE := regexp.MustCompile(`(?is)^\s*CREATE TABLE IF NOT EXISTS (\w+)`)
	for _, m := range contentDeskMigrations {
		if seen[m.name] || existing[m.name] {
			t.Errorf("migration name %q collides", m.name)
		}
		seen[m.name] = true
		body := strings.TrimSpace(m.sql)
		isBlock := strings.HasPrefix(body, "DO $$") || strings.HasPrefix(body, "CREATE OR REPLACE FUNCTION")
		if !isBlock && strings.Contains(body, ";") {
			t.Errorf("%s: one statement per entry (migrationSkipProbe classifies by leading keyword)", m.name)
		}
		if mm := createRE.FindStringSubmatch(body); mm != nil {
			created[mm[1]] = true
			if !strings.Contains(body, "org_id") {
				t.Errorf("%s: every content table carries org_id", m.name)
			}
			if kind, ident, _ := classifyMigrationStatement(body); kind != migStmtCreateTable || ident != mm[1] {
				t.Errorf("%s: skip probe must classify it as CREATE TABLE %s", m.name, mm[1])
			}
		}
		if strings.Contains(strings.ToUpper(body), "ALTER TABLE MAILING_") || strings.Contains(body, "mailing_campaign") {
			t.Errorf("%s: the content desk must not touch mailing_* (send-path) tables", m.name)
		}
	}
	for _, tb := range contentDeskTables {
		if !created[tb] {
			t.Errorf("no migration creates %s", tb)
		}
	}

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	iCF := strings.Index(body, "migrations = append(migrations, contractFulfillmentMigrations...)")
	iCD := strings.Index(body, "migrations = append(migrations, contentDeskMigrations...)")
	if iCD < 0 || iCD < iCF {
		t.Fatal("contentDeskMigrations must be appended to runStartupMigrations after contractFulfillmentMigrations")
	}
	if !strings.Contains(body, "worker.NewContentDeskWorker(mailingDB, redisClient)") || !strings.Contains(body, "contentDeskWorker.Start(ctx)") {
		t.Fatal("ContentDeskWorker must be started at boot")
	}
}

func TestContentDeskSeed_27PropertiesBySurface(t *testing.T) {
	var seed string
	for _, m := range contentDeskMigrations {
		if m.name == "content_desk_seed_sites" {
			seed = m.sql
		}
	}
	rowRE := regexp.MustCompile(`\('([A-Z]+)',\s*'([a-z0-9.-]+)',\s*'(static|mongo|qf)',\s*(TRUE|FALSE)\)`)
	rows := rowRE.FindAllStringSubmatch(seed, -1)
	if len(rows) != 27 {
		t.Fatalf("want 27 seeded properties, got %d", len(rows))
	}
	count := map[string]int{}
	for _, r := range rows {
		count[r[3]]++
		if r[3] == "qf" && (r[2] != "quizfiesta.com" || r[4] != "FALSE") {
			t.Errorf("quizfiesta must be the only qf surface and disabled: %v", r)
		}
		if r[3] == "mongo" && !map[string]bool{"historythinking.com": true, "myownhealth.net": true, "discountblog.com": true}[r[2]] {
			t.Errorf("unexpected mongo site %s", r[2])
		}
	}
	if count["static"] != 23 || count["mongo"] != 3 || count["qf"] != 1 {
		t.Fatalf("surface split = %v, want 23/3/1", count)
	}
}
