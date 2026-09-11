package api

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

var (
	testDrops = []string{
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_send_type_check`,
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check`,
	}
	testAdds = []string{
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS list_ids JSONB DEFAULT '[]'`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS queued_count INTEGER DEFAULT 0`,
	}
)

// pg_get_constraintdef renders values as 'draft'::character varying.
func coveringStatusDef() string {
	var parts []string
	for _, v := range campaignStatusValues {
		parts = append(parts, "'"+v+"'::character varying")
	}
	return "CHECK (((status)::text = ANY ((ARRAY[" + strings.Join(parts, ", ") + "])::text[])))"
}

// 2026-09-11: on an up-to-date table the boot path must issue NO DDL — every
// no-op ALTER here queues ACCESS EXCLUSIVE on the send path.
func TestCampaignSchemaPlan_UpToDateIssuesNothing(t *testing.T) {
	cols := map[string]bool{"list_ids": true, "queued_count": true}
	cons := map[string]string{campaignStatusCheck: coveringStatusDef()}
	if p := campaignSchemaPlan(testDrops, testAdds, cols, cons); len(p) != 0 {
		t.Fatalf("up-to-date table must plan no DDL, got %v", p)
	}
}

func TestCampaignSchemaPlan_OnlyWhatIsMissing(t *testing.T) {
	// No status CHECK (prod's state): it stays absent — never re-added at boot.
	fresh := campaignSchemaPlan(testDrops, testAdds, map[string]bool{}, map[string]string{})
	if want := []string{testAdds[0], testAdds[1]}; !reflect.DeepEqual(fresh, want) {
		t.Fatalf("fresh table:\n got %v\nwant %v", fresh, want)
	}
	// Prod as probed 2026-09-11: every column present, no status CHECK → zero DDL.
	all := map[string]bool{"list_ids": true, "queued_count": true}
	if p := campaignSchemaPlan(testDrops, testAdds, all, map[string]string{}); len(p) != 0 {
		t.Fatalf("prod-shaped table must plan no DDL, got %v", p)
	}
	cons := map[string]string{
		"mailing_campaigns_send_type_check": "CHECK (send_type IN ('blast'))",
		campaignStatusCheck:                 "CHECK (((status)::text = ANY ((ARRAY['draft'::character varying])::text[])))",
	}
	got := campaignSchemaPlan(testDrops, testAdds, map[string]bool{"list_ids": true}, cons)
	want := []string{testDrops[0], testAdds[1],
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS ` + campaignStatusCheck, campaignStatusCheckDDL()}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stale table:\n got %v\nwant %v", got, want)
	}
}

func TestEnsureCampaignColumns_NoRawDDL(t *testing.T) {
	src, err := os.ReadFile("campaign_builder_helpers.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"cb.db.ExecContext(ctx, ddl)", "cb.db.ExecContext(ctx, migration)", "ADD CONSTRAINT mailing_campaigns_status_check"} {
		if strings.Contains(string(src), bad) {
			t.Fatalf("ensureCampaignColumns still runs raw DDL (%q): route it through applyCampaignSchema", bad)
		}
	}
}
