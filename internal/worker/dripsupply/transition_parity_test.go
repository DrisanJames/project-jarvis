package dripsupply

// SQL-fragment parity.
//
// transition.go DUPLICATES four fragments that live in internal/worker and one
// constant that lives in cmd/server/main.go. It has to: internal/worker imports
// THIS package (WP5 injects the service into PartnerDripOrchestrator), so
// importing internal/worker here would be a cycle — and every one of the
// duplicated helpers is unexported over there, so even an external
// `dripsupply_test` package could not reach them.
//
// So parity is pinned the only way that actually works from inside this
// package: by reading the source files off disk and asserting the fragment is
// still there, verbatim. When the orchestrator's copy changes, these tests fail
// and the next person has to decide deliberately whether this copy follows —
// which is the whole point. A missing file is a FAILURE, not a skip: these
// paths are in-repo and a rename must not silently disarm the guard.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	orchestratorSrc = "../partner_drip_orchestrator.go"
	convertersSrc   = "../partner_drip_converters.go"
	engGateSrc      = "../partner_drip_engagement_gate.go"
	mainSrc         = "../../../cmd/server/main.go"
)

func readSource(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("resolve %s: %v", rel, err)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("parity source %s unreadable (%v) — the file moved and this guard is disarmed; repoint it", abs, err)
	}
	return string(b)
}

// mustContain asserts a duplicated fragment is still present in its source.
func mustContain(t *testing.T, src, path, label, fragment string) {
	t.Helper()
	if !strings.Contains(src, fragment) {
		t.Errorf("%s has DRIFTED from %s.\n"+
			"The copy in transition.go is no longer in the source. Re-read the source and decide\n"+
			"whether this package follows it.\nlocal copy:\n%s", label, path, fragment)
	}
}

// TestDatasetNotEmergencyPausedSQLParity pins the REQ-004 emergency-stop
// predicate against internal/worker/partner_drip_orchestrator.go:2194.
func TestDatasetNotEmergencyPausedSQLParity(t *testing.T) {
	mustContain(t, readSource(t, orchestratorSrc), orchestratorSrc,
		"datasetNotEmergencyPausedSQL", datasetNotEmergencyPausedSQL)
}

// TestHomeBrandPinSQLParity pins the converter home-brand pin against
// internal/worker/partner_drip_converters.go:71. The function body is compared
// through its OUTPUT for both branches, which is what the claim SQL actually
// embeds, plus the literal expression from the source.
func TestHomeBrandPinSQLParity(t *testing.T) {
	src := readSource(t, convertersSrc)

	mustContain(t, src, convertersSrc, "convertersPrefix",
		`convertersPrefix = "`+convertersPrefix+`"`)
	mustContain(t, src, convertersSrc, "homeBrandPinSQL expression",
		`return "\n\t\t\t  AND (COALESCE(" + q + "extra_metadata->>'home_brand','') = '' OR lower(" + q + "extra_metadata->>'home_brand') = " + b + ")"`)
	mustContain(t, src, convertersSrc, "convertersPinDisabled",
		`v := os.Getenv("PARTNER_DRIP_CONVERTERS_PIN_DISABLED")`)

	// Behavioural parity of the port itself.
	if got := homeBrandPinSQL("wcl_remail", "ht", ""); got != "" {
		t.Errorf("homeBrandPinSQL on a non-converter vertical = %q, want \"\" (byte-identical legacy SQL)", got)
	}
	got := homeBrandPinSQL("converters_auto", "HT", "q")
	want := "\n\t\t\t  AND (COALESCE(q.extra_metadata->>'home_brand','') = '' OR lower(q.extra_metadata->>'home_brand') = 'ht')"
	if got != want {
		t.Errorf("homeBrandPinSQL(converters_auto, HT, q):\n got %q\nwant %q", got, want)
	}
	if got := homeBrandPinSQL("converters_auto", "o'brien", ""); !strings.Contains(got, "'o''brien'") {
		t.Errorf("quoteSQLLiteral escaping lost: %q", got)
	}

	// The kill switch is read at call time, not cached.
	t.Setenv("PARTNER_DRIP_CONVERTERS_PIN_DISABLED", "1")
	if got := homeBrandPinSQL("converters_auto", "ht", ""); got != "" {
		t.Errorf("PARTNER_DRIP_CONVERTERS_PIN_DISABLED=1 did not disable the pin: %q", got)
	}
}

// TestQuoteSQLLiteralParity pins the escaping helper against
// internal/worker/partner_drip_engagement_gate.go:307.
func TestQuoteSQLLiteralParity(t *testing.T) {
	mustContain(t, readSource(t, engGateSrc), engGateSrc, "quoteSQLLiteral",
		`return "'" + strings.ReplaceAll(s, "'", "''") + "'"`)
}

// TestCapsValuesClausesParity pins the caps VALUES builder against
// internal/worker/partner_drip_orchestrator.go:2940 — specifically that the
// source still emits a row for a ZERO cap, which is what keeps a 0-grant ISP
// out of the 'other' bucket.
func TestCapsValuesClausesParity(t *testing.T) {
	src := readSource(t, orchestratorSrc)
	mustContain(t, src, orchestratorSrc, "capsValuesClauses placeholder shape",
		`clauses = append(clauses, fmt.Sprintf("($%d::text, $%d::int)", idx, idx+1))`)
	mustContain(t, src, orchestratorSrc, "capsValuesClauses zero-cap handling",
		"\t\tif capValue < 0 {\n\t\t\tcapValue = 0\n\t\t}")

	clauses, args, positive := capsValuesClauses(map[string]int{"aol": 0}, 4)
	if len(clauses) != 1 || clauses[0] != "($4::text, $5::int)" {
		t.Errorf("clauses = %v, want one ($4::text, $5::int) — a dropped zero row re-opens the 'other'-bucket leak", clauses)
	}
	if positive != 0 {
		t.Errorf("positive = %d for an all-zero map, want 0", positive)
	}
	if len(args) != 2 || args[0] != "aol" || args[1] != 0 {
		t.Errorf("args = %v, want [aol 0]", args)
	}
	if _, _, positive := capsValuesClauses(map[string]int{"aol": -5}, 4); positive != 0 {
		t.Error("a negative cap counted as positive")
	}
}

// TestClaimCTEParityWithOrchestrator pins the ranked/eligible/picked CTE shape
// against the orchestrator's claimRecordsByISPCaps
// (partner_drip_orchestrator.go:2959-3051). ClaimByISPCaps changes exactly two
// things — the caps source and the allocation stamp — and this test is what
// makes "exactly two" checkable.
func TestClaimCTEParityWithOrchestrator(t *testing.T) {
	src := readSource(t, orchestratorSrc)
	for label, fragment := range map[string]string{
		"other-bucket fallback": `WHEN COALESCE(NULLIF(isp_family, ''), 'other') IN (SELECT isp FROM caps)
			               THEN COALESCE(NULLIF(isp_family, ''), 'other') ELSE 'other' END`,
		"oldest-ingest-first window": `ORDER BY ingested_at ASC
			       ) AS rn`,
		"cap join": `JOIN caps c ON c.isp = r.isp_bucket
			WHERE r.rn <= c.cap`,
		"picked lock":    `FOR UPDATE SKIP LOCKED`,
		"returning list": `RETURNING q.id, q.email, q.email_md5, q.isp_family, q.dataset_id, q.partner_id, q.batch_id, q.extra_metadata`,
	} {
		mustContain(t, src, orchestratorSrc, "claimRecordsByISPCaps "+label, fragment)
	}

	// And the orchestrator's UPDATE must still be the UNSTAMPED one: WP4 does
	// not touch it (WP5 rewires the caller). If this fails, someone edited the
	// old path and the two claim paths now disagree about the allocation.
	mustContain(t, src, orchestratorSrc, "claimRecordsByISPCaps UPDATE (untouched by WP4)",
		"\t\tUPDATE partner_clean_queue q\n\t\tSET status = 'claimed', claimed_at = NOW()\n\t\tFROM picked")
}

// TestReleaseClaimSQLParity pins the release CASE against
// internal/worker/partner_drip_orchestrator.go:3190-3215. The CASE is the
// 2026-08-05 ladder-ejection guard; a "simplification" of it on either side is
// a production incident.
func TestReleaseClaimSQLParity(t *testing.T) {
	mustContain(t, readSource(t, orchestratorSrc), orchestratorSrc, "releaseClaim CASE",
		`SET status = CASE
		        WHEN COALESCE(touch_count, 0) >= 1 AND mailed_campaign_id IS NOT NULL
		        THEN 'mailed'
		        ELSE 'ready'
		    END,
		    claimed_at = NULL`)
}

// TestPCQAllocationFenceParity pins this package's copy of the fence literal and
// of the CHECK expression against WP1's criticalSendPathDDL entry in
// cmd/server/main.go. The integration tests build the constraint from
// PCQClaimConstraintDDL, so if that drifts from what production applies, test 8
// would be exercising a constraint the database does not have.
func TestPCQAllocationFenceParity(t *testing.T) {
	src := readSource(t, mainSrc)
	mustContain(t, src, mainSrc, "pcqAllocationFence",
		`const pcqAllocationFence = "`+PCQAllocationFence+`"`)
	mustContain(t, src, mainSrc, "pcq_claim_requires_allocation CHECK",
		"CHECK (status <> 'claimed' OR capacity_allocation_id IS NOT NULL OR claimed_at < '` + pcqAllocationFence + `') NOT VALID")

	ddl := PCQClaimConstraintDDL(PCQAllocationFence)
	if !strings.Contains(ddl, "ADD CONSTRAINT pcq_claim_requires_allocation") {
		t.Errorf("PCQClaimConstraintDDL lost the production constraint NAME:\n%s", ddl)
	}
	if !strings.Contains(ddl, "NOT VALID") {
		t.Errorf("PCQClaimConstraintDDL lost NOT VALID — a validating ADD would scan 13.5M rows at boot:\n%s", ddl)
	}
	if !strings.Contains(ddl, "'"+PCQAllocationFence+"'") {
		t.Errorf("PCQClaimConstraintDDL did not embed the fence:\n%s", ddl)
	}
	// The rewrite the integration tests rely on.
	if strings.Contains(PCQClaimConstraintDDL(pastFence), PCQAllocationFence) {
		t.Error("PCQClaimConstraintDDL(pastFence) still carries the shipped fence")
	}
}

// TestClaimedRecordMirrorsOrchestratorShape pins ClaimedRecord against
// worker.claimedRecord (partner_drip_orchestrator.go:2171): the RETURNING list
// and the scan targets must stay in the same order and count, or WP5's adapter
// silently transposes columns.
func TestClaimedRecordMirrorsOrchestratorShape(t *testing.T) {
	mustContain(t, readSource(t, orchestratorSrc), orchestratorSrc, "claimedRecord fields",
		`type claimedRecord struct {
	id        string
	email     string
	emailMD5  string
	ispFamily string
	datasetID string
	partnerID string
	batchID   string
	extra     []byte
}`)
}
