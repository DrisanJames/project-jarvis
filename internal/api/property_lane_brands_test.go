package api

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The roster table has ALREADY drifted from the compiled slice: 'wcl' mails the
// refi_heloc lane but is absent from dripBrands. These tests pin the union so a
// future refactor cannot quietly re-narrow lane validation to the compiled set.

func TestLaneBrand_UnionAcceptsRosterOnlyBrand(t *testing.T) {
	invalidateLaneBrandCache()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("FROM partner_drip_vertical_roster").
		WillReturnRows(sqlmock.NewRows([]string{"brand"}).AddRow("wcl").AddRow("db"))

	if !propertyLedgerValidLaneBrand(context.Background(), db, "wcl") {
		t.Fatal("wcl is actively rostered on refi_heloc — must validate, else a live lane 400s")
	}
	invalidateLaneBrandCache()
	mock.ExpectQuery("FROM partner_drip_vertical_roster").
		WillReturnRows(sqlmock.NewRows([]string{"brand"}).AddRow("wcl"))
	if !propertyLedgerValidLaneBrand(context.Background(), db, "db") {
		t.Fatal("db is a compiled drip brand — the union must keep it even when absent from the row set")
	}
}

// NEGATIVE PATH: a DB failure must DEGRADE to the compiled set, never reject
// everything. A validator that closes on error would take the whole surface down.
func TestLaneBrand_DBErrorFallsBackToCompiled(t *testing.T) {
	invalidateLaneBrandCache()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("FROM partner_drip_vertical_roster").
		WillReturnError(errors.New("connection refused"))

	if !propertyLedgerValidLaneBrand(context.Background(), db, "db") {
		t.Fatal("compiled brand must still validate when the roster query fails")
	}
	invalidateLaneBrandCache()
	mock.ExpectQuery("FROM partner_drip_vertical_roster").
		WillReturnError(errors.New("connection refused"))
	if propertyLedgerValidLaneBrand(context.Background(), db, "wcl") {
		t.Fatal("roster-only brand must NOT validate when the roster is unreadable — that is honest degradation")
	}
}

func TestLaneBrand_RejectsEmptyAndUnknown(t *testing.T) {
	invalidateLaneBrandCache()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("FROM partner_drip_vertical_roster").
		WillReturnRows(sqlmock.NewRows([]string{"brand"}).AddRow("db"))

	for _, bad := range []string{"", "   ", "notabrand", "DROP TABLE"} {
		if propertyLedgerValidLaneBrand(context.Background(), db, bad) {
			t.Fatalf("%q must not validate", bad)
		}
	}
}

// Case/whitespace normalization: the roster stores lower(btrim(brand)) and the
// orchestrator compares lowered, so validation must match that contract.
func TestLaneBrand_NormalizesInput(t *testing.T) {
	invalidateLaneBrandCache()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("FROM partner_drip_vertical_roster").
		WillReturnRows(sqlmock.NewRows([]string{"brand"}).AddRow("wcl"))

	if !propertyLedgerValidLaneBrand(context.Background(), db, "  WCL  ") {
		t.Fatal("brand validation must trim and lowercase to match the roster's stored form")
	}
}

// NEGATIVE CONTROL. mailing_brand_metadata.brand_code is the PYTHON registry's
// scheme (BW, HW, LP, MR, TT, WF, YI, RR); the drip roster uses bwp, hws, lpl,
// mrd, tot, wfy, yih, rru. A future "let's also accept known brands" union with
// that table would admit codes the orchestrator cannot route, silently creating
// roster rows that never mail. This test fails if anyone does that.
func TestLaneBrand_DoesNotAdmitRegistryCodeScheme(t *testing.T) {
	invalidateLaneBrandCache()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Roster answers with only the real drip codes, as production does.
	for range []int{0, 1, 2, 3, 4, 5, 6, 7} {
		mock.ExpectQuery("FROM partner_drip_vertical_roster").
			WillReturnRows(sqlmock.NewRows([]string{"brand"}).AddRow("wcl").AddRow("db"))
	}
	for _, registryOnly := range []string{"bw", "hw", "lp", "mr", "tt", "wf", "yi", "rr"} {
		invalidateLaneBrandCache()
		if propertyLedgerValidLaneBrand(context.Background(), db, registryOnly) {
			t.Fatalf("%q is a mailing_brand_metadata registry code, not a drip roster code — "+
				"accepting it would create a roster row the orchestrator cannot route", registryOnly)
		}
	}
	// ...while the genuine drip code for the same brand still validates.
	if !propertyLedgerValidLaneBrand(context.Background(), db, "db") {
		t.Fatal("real drip code must still validate")
	}
}
