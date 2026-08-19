package worker

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNamesFromExtra(t *testing.T) {
	cases := []struct {
		name        string
		extra       string
		wantFirst   string
		wantLast    string
	}{
		{"both present", `{"first_name":"Derek","last_name":"Delfino","state":"FL"}`, "Derek", "Delfino"},
		{"api door empty strings", `{"first_name":"","last_name":"","zip":"","metadata":null}`, "", ""},
		{"keys absent", `{"source":"drivesource","drive_file_id":"x"}`, "", ""},
		{"nil extra", "", "", ""},
		{"invalid json", `{not json`, "", ""},
		{"whitespace only", `{"first_name":"   "}`, "", ""},
		{"trims", `{"first_name":" Ann ","last_name":" Lee "}`, "Ann", "Lee"},
		{"caps at 100 runes", `{"first_name":"` + strings.Repeat("a", 150) + `"}`, strings.Repeat("a", 100), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var extra []byte
			if c.extra != "" {
				extra = []byte(c.extra)
			}
			first, last := namesFromExtra(extra)
			if first != c.wantFirst || last != c.wantLast {
				t.Fatalf("namesFromExtra(%q) = (%q,%q), want (%q,%q)",
					c.extra, first, last, c.wantFirst, c.wantLast)
			}
		})
	}
}

// TestPersonaFieldsFromExtra pins the extraction contract for the geo/vehicle
// personalization the internal_auto_insurance creative renders. Before this,
// promoteToSubscribers carried ONLY first_name/last_name onto the subscriber,
// so {{ custom.city }} / {{ custom.postal_code }} / {{ custom.vehicle }} always
// fell through to their Liquid `| default:` values.
//
// The two ingest doors disagree on nesting (partner_slicer.go writes state/zip
// at the top level; agents/drivesource/pull.py routes city + lead detail into
// the nested "metadata" object), so both shapes are pinned here.
func TestPersonaFieldsFromExtra(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  map[string]interface{}
	}{
		{
			"api door top level",
			`{"first_name":"Derek","state":"FL","zip":"33021","source":"autocoveragepoint"}`,
			map[string]interface{}{"state": "FL", "postal_code": "33021"},
		},
		{
			"nested metadata door",
			`{"first_name":"Ann","metadata":{"city":"Boise","vehicle":"2019 Honda Civic","tid":"ff2007"}}`,
			map[string]interface{}{"city": "Boise", "vehicle": "2019 Honda Civic", "tid": "ff2007"},
		},
		{
			"full posted payload",
			`{"email":"a@x.com","first_name":"Derek","last_name":"Delfino","city":"Hollywood",` +
				`"state":"FL","postal_code":"33021","source":"acp",` +
				`"metadata":{"vehicle":"2021 Ford F-150","tid":"7552"}}`,
			map[string]interface{}{
				"city": "Hollywood", "state": "FL",
				"postal_code": "33021", "vehicle": "2021 Ford F-150",
				"tid": "7552",
			},
		},
		// postal_code beats zip beats metadata.zip; top level always wins.
		{
			"top level wins over nested",
			`{"city":"Denver","zip":"80202","metadata":{"city":"Aurora","zip":"80010"}}`,
			map[string]interface{}{"city": "Denver", "postal_code": "80202"},
		},
		{"zip fallback from metadata", `{"metadata":{"zip":"80202"}}`, map[string]interface{}{"postal_code": "80202"}},
		// A zip that arrives as a JSON number is still a zip.
		{"numeric zip", `{"metadata":{"zip":80202}}`, map[string]interface{}{"postal_code": "80202"}},

		// ---- every one of these must yield an EMPTY, NON-NIL map: it marshals
		// to '{}' and merges as a no-op, so the insert can never fail. ----
		{"nil extra", "", map[string]interface{}{}},
		{"invalid json", `{not json`, map[string]interface{}{}},
		{"api door empty strings", `{"first_name":"","zip":"","state":"","metadata":null}`, map[string]interface{}{}},
		{"whitespace only", `{"city":"   "}`, map[string]interface{}{}},
		{"names only", `{"first_name":"Derek","last_name":"Delfino"}`, map[string]interface{}{}},
		// metadata that is not an object must not discard the top-level fields.
		{"metadata is a string", `{"state":"TX","metadata":"n/a"}`, map[string]interface{}{"state": "TX"}},
		{"metadata is an array", `{"state":"TX","metadata":[1,2]}`, map[string]interface{}{"state": "TX"}},
		{"nested non-string value", `{"metadata":{"vehicle":{"make":"Ford"}}}`, map[string]interface{}{}},
		// VARCHAR-width discipline, same 100-rune cap as namesFromExtra.
		{
			"caps at 100 runes",
			`{"city":"` + strings.Repeat("a", 150) + `"}`,
			map[string]interface{}{"city": strings.Repeat("a", 100)},
		},
		{"trims", `{"city":"  Boise  "}`, map[string]interface{}{"city": "Boise"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var extra []byte
			if c.extra != "" {
				extra = []byte(c.extra)
			}
			got := personaFieldsFromExtra(extra)
			if got == nil {
				t.Fatalf("personaFieldsFromExtra(%q) returned nil; must be an empty map so it marshals to '{}'", c.extra)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("personaFieldsFromExtra(%q) = %#v, want %#v", c.extra, got, c.want)
			}
			// Whatever comes out must be bindable to a ::jsonb parameter.
			if _, err := json.Marshal(got); err != nil {
				t.Fatalf("personaFieldsFromExtra(%q) is not marshalable: %v", c.extra, err)
			}
		})
	}
}

// TestPromoteToSubscribersCarriesCustomFields is the end-to-end guard: the
// extracted persona has to actually reach mailing_subscribers.custom_fields —
// the column the send worker's claim query selects (send_worker.go:872) and
// buildRenderContext exposes as rc["custom"] (send_worker.go:3134). A pure
// extractor test would still pass if the INSERT never carried the value.
//
// Also pins the ON CONFLICT semantics, because promoteToSubscribers re-runs on
// every follow-up touch: the geo fields MERGE into whatever another door wrote
// (`|| EXCLUDED.custom_fields`), they never clobber the whole column.
func TestPromoteToSubscribersCarriesCustomFields(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db}
	po.cfg.OrganizationID = "00000000-0000-0000-0000-000000000001"

	insertRe := `(?s)INSERT INTO mailing_subscribers.*first_name, last_name, custom_fields\).*` +
		`\$12::jsonb.*custom_fields = COALESCE\(mailing_subscribers\.custom_fields, '\{\}'::jsonb\) \|\| EXCLUDED\.custom_fields`

	mock.ExpectBegin()
	prep := mock.ExpectPrepare(insertRe)
	prep.ExpectQuery().
		WithArgs(
			sqlmock.AnyArg(), po.cfg.OrganizationID, dataPartnerMasterListID,
			"derek@example.com", "md5hash",
			"data_partner", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"Derek", "Delfino",
			`{"city":"Hollywood","postal_code":"33021","state":"FL","tid":"7552","vehicle":"2021 Ford F-150"}`,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sub-1"))
	mock.ExpectCommit()
	// linkSubscriberIDsToQueue best-effort write-back.
	mock.ExpectExec(`UPDATE partner_clean_queue`).WillReturnResult(sqlmock.NewResult(0, 1))

	recs := []claimedRecord{{
		id:       "11111111-1111-1111-1111-111111111111",
		email:    "derek@example.com",
		emailMD5: "md5hash",
		batchID:  "b-1",
		extra: []byte(`{"email":"derek@example.com","first_name":"Derek","last_name":"Delfino",` +
			`"city":"Hollywood","state":"FL","postal_code":"33021","source":"acp",` +
			`"metadata":{"vehicle":"2021 Ford F-150","tid":"7552"}}`),
	}}

	ids, err := po.promoteToSubscribers(context.Background(), verticalState{
		vertical: "internal_auto_insurance", partnerSlug: "acp", datasetSlug: "acp-auto",
	}, recs)
	require.NoError(t, err)
	require.Equal(t, []string{"sub-1"}, ids)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// A record with no geo data at all must still bind a valid '{}' — an empty
// string would fail the ::jsonb cast and drop the recipient from the wave.
func TestPromoteToSubscribersEmptyPersonaBindsEmptyObject(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db}
	po.cfg.OrganizationID = "00000000-0000-0000-0000-000000000001"

	mock.ExpectBegin()
	prep := mock.ExpectPrepare(`INSERT INTO mailing_subscribers`)
	prep.ExpectQuery().
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"nogeo@example.com", "md5hash",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"", "",
			"{}",
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("sub-2"))
	mock.ExpectCommit()
	mock.ExpectExec(`UPDATE partner_clean_queue`).WillReturnResult(sqlmock.NewResult(0, 1))

	ids, err := po.promoteToSubscribers(context.Background(), verticalState{vertical: "internal_auto_insurance"},
		[]claimedRecord{{
			id:       "22222222-2222-2222-2222-222222222222",
			email:    "nogeo@example.com",
			emailMD5: "md5hash",
			extra:    []byte(`{not json`),
		}})
	require.NoError(t, err)
	require.Equal(t, []string{"sub-2"}, ids)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Regression guard for the 2026-08-19 loan-detail drop.
//
// The WCL Mortgage 08-18-26 feed posts loan_type / property_type inside the
// record's nested metadata, and partner_clean_queue stored both on every row.
// personaFieldsFromExtra's allow-list did not carry them, so of 6,071 promoted
// subscribers exactly 2 had loan_type in custom_fields and ZERO had
// property_type. The v6 HELOC creatives render both, guarded by
// `{% assign p_loan = custom.loan_type | default: "" %}` + `{% if p_loan != "" %}`
// — so the branch simply never fired and the mail shipped without the clause,
// with every render test still green against a synthetic persona.
func TestPersonaFieldsFromExtra_CarriesLoanDetail(t *testing.T) {
	extra := []byte(`{
		"city": "Blue Springs", "state": "MO", "zip": "64015",
		"metadata": {
			"loan_type": "Conventional",
			"property_type": "Condominium",
			"credit_rating": "good"
		}
	}`)
	got := personaFieldsFromExtra(extra)

	if got["loan_type"] != "Conventional" {
		t.Fatalf("loan_type not carried: %#v", got["loan_type"])
	}
	if got["property_type"] != "Condominium" {
		t.Fatalf("property_type not carried: %#v", got["property_type"])
	}
	// Unchanged fields must still land.
	if got["city"] != "Blue Springs" || got["state"] != "MO" {
		t.Fatalf("existing geo fields regressed: %#v", got)
	}
	// Keys NOT on the allow-list stay off it — this is an allow-list, not a
	// passthrough, and widening it silently is how PII leaks into custom_fields.
	if _, ok := got["credit_rating"]; ok {
		t.Fatalf("credit_rating must not be emitted: %#v", got)
	}
}

// A feed carrying neither key must be byte-identical to before the change.
func TestPersonaFieldsFromExtra_NoLoanDetailIsUnchanged(t *testing.T) {
	got := personaFieldsFromExtra([]byte(`{"city":"Boise","state":"ID"}`))
	if _, ok := got["loan_type"]; ok {
		t.Fatalf("loan_type must be absent when not supplied: %#v", got)
	}
	if _, ok := got["property_type"]; ok {
		t.Fatalf("property_type must be absent when not supplied: %#v", got)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected extra keys: %#v", got)
	}
}

// equity_estimate is DERIVED — no upstream system ships it — so it is pinned
// hard. Rounding is DOWN so the figure never exceeds what the recipient's own
// stated numbers support, and the key is ABSENT rather than zero when we
// cannot stand behind it, which makes the creative drop the clause entirely.
func TestEquityEstimate(t *testing.T) {
	cases := []struct {
		name, pv, lb, want string
		ok                 bool
	}{
		{"rounds down to nearest 10k", "1096031", "487496", "600,000", true},
		{"exact multiple survives", "500000", "200000", "300,000", true},
		{"commas tolerated", "1,096,031", "487,496", "600,000", true},
		{"whitespace tolerated", " 500000 ", " 200000 ", "300,000", true},
		{"zero balance is full value", "250000", "0", "250,000", true},
		{"under 10k equity is withheld", "205000", "200000", "", false},
		{"exactly 10k qualifies", "210000", "200000", "10,000", true},
		{"underwater is withheld", "200000", "260000", "", false},
		{"missing property value", "", "200000", "", false},
		{"missing balance", "500000", "", "", false},
		{"garbage input", "n/a", "200000", "", false},
		{"negative property value", "-500000", "1000", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := equityEstimate(c.pv, c.lb)
			if ok != c.ok || got != c.want {
				t.Fatalf("equityEstimate(%q,%q) = (%q,%v), want (%q,%v)",
					c.pv, c.lb, got, ok, c.want, c.ok)
			}
		})
	}
}

// The full extraction path: a real feed payload must yield the derived equity
// and the stated intent alongside the geo fields.
func TestPersonaFieldsFromExtra_DerivesEquityAndPurpose(t *testing.T) {
	extra := []byte(`{
		"city": "Blue Springs", "state": "MO",
		"metadata": {
			"property_value": "1096031", "loan_balance": "487496",
			"loan_purpose": "HELOC", "loan_type": "Conventional",
			"credit_rating": "good", "phone": "7048959486"
		}
	}`)
	got := personaFieldsFromExtra(extra)

	if got["equity_estimate"] != "600,000" {
		t.Fatalf("equity_estimate = %#v, want \"600,000\"", got["equity_estimate"])
	}
	if got["loan_purpose"] != "HELOC" {
		t.Fatalf("loan_purpose = %#v", got["loan_purpose"])
	}
	// credit_rating and phone must NOT reach custom_fields: a credit tier read
	// back to a consumer is a compliance exposure, and the phone number has no
	// business in a template context.
	for _, k := range []string{"credit_rating", "phone", "property_value", "loan_balance"} {
		if _, ok := got[k]; ok {
			t.Fatalf("%s must not be emitted into custom_fields: %#v", k, got)
		}
	}
}

// An underwater or thin-equity record emits no key at all, so the creative's
// guard drops the clause rather than rendering "$0" or an empty slot.
func TestPersonaFieldsFromExtra_ThinEquityEmitsNoKey(t *testing.T) {
	got := personaFieldsFromExtra([]byte(
		`{"metadata":{"property_value":"205000","loan_balance":"200000"}}`))
	if _, ok := got["equity_estimate"]; ok {
		t.Fatalf("thin equity must emit no key: %#v", got)
	}
}
