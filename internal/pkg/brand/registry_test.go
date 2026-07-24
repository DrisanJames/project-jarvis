package brand

// DB-backed owned-domains registry tests (audit G6). The key invariant is
// the NEGATIVE path (AS-7.1): when the table is absent/unreadable, Domains()
// and Root() behave byte-identically to the compiled-in OwnedDomains list.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDomainsFallbackWhenDBFails(t *testing.T) {
	resetRegistryForTest()
	t.Cleanup(resetRegistryForTest)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT domain FROM mailing_owned_domains`).
		WillReturnError(errors.New(`pq: relation "mailing_owned_domains" does not exist`))

	if err := RefreshFromDB(context.Background(), db); err == nil {
		t.Fatal("RefreshFromDB must surface the table-absent error")
	}
	if !reflect.DeepEqual(Domains(), OwnedDomains) {
		t.Fatalf("after a failed refresh Domains() must equal the hardcoded list")
	}
	if got := Root("em.discountblog.com"); got != "discountblog.com" {
		t.Fatalf("Root fallback broken: %q", got)
	}
}

func TestRefreshFromDBUnionsWithHardcodedList(t *testing.T) {
	resetRegistryForTest()
	t.Cleanup(resetRegistryForTest)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT domain FROM mailing_owned_domains`).
		WillReturnRows(sqlmock.NewRows([]string{"domain"}).
			AddRow("discountblog.com").   // duplicate of hardcoded — must not double
			AddRow("Freshly-Onboarded.com")) // DB-only domain, mixed case

	if err := RefreshFromDB(context.Background(), db); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got := Domains()
	if len(got) != len(OwnedDomains)+1 {
		t.Fatalf("union sizing wrong: got %d, want %d", len(got), len(OwnedDomains)+1)
	}
	// Hardcoded list must survive intact (union, never replacement).
	for i, od := range OwnedDomains {
		if got[i] != od {
			t.Fatalf("hardcoded domain %q lost/reordered at %d (got %q)", od, i, got[i])
		}
	}
	if got := Root("em.freshly-onboarded.com"); got != "freshly-onboarded.com" {
		t.Fatalf("Root must consult the DB-loaded list, got %q", got)
	}
}

func TestRegisterIsImmediateAndIdempotent(t *testing.T) {
	resetRegistryForTest()
	t.Cleanup(resetRegistryForTest)

	Register("New-Brand.com")
	Register("new-brand.com") // duplicate

	if got := Root("t.em.new-brand.com"); got != "new-brand.com" {
		t.Fatalf("Register must take effect immediately, got %q", got)
	}
	if len(Domains()) != len(OwnedDomains)+1 {
		t.Fatalf("Register must be idempotent: %d domains", len(Domains()))
	}
}

func TestWCLHelocIsSeeded(t *testing.T) {
	// The jul-2026 first-party addition must be in the compiled-in seed so
	// the DB seed (generated from this list) and the fallback both carry it.
	if Root("em.wcl-heloc.com") != "wcl-heloc.com" {
		t.Fatal("wcl-heloc.com missing from OwnedDomains")
	}
}
