package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsSuppressedForBrand verifies the matrix of:
//
//	(global hit, brand hit) x (brandRoot empty vs set)
//
// against the single-read-lock implementation. This is the hottest call on
// the send path; a regression here silently breaks deliverability.
func TestIsSuppressedForBrand(t *testing.T) {
	h := NewGlobalSuppressionHub(nil, "org-1", "")

	globalEmail := "global@example.com"
	brandEmail := "brand-only@example.com"
	otherEmail := "clean@example.com"

	// Seed state as LoadFromDB would.
	h.emailSet[globalEmail] = true
	h.hashSet[MD5Hash(globalEmail)] = true
	h.brandSet["discountblog.com"] = map[string]bool{
		MD5Hash(brandEmail): true,
	}

	cases := []struct {
		name      string
		email     string
		brandRoot string
		want      bool
	}{
		{"global hit, no brand param", globalEmail, "", true},
		{"global hit wins over brand miss", globalEmail, "quizfiesta.com", true},
		{"brand hit for matching brand", brandEmail, "discountblog.com", true},
		{"brand entry does NOT leak to other brand", brandEmail, "quizfiesta.com", false},
		{"brand entry ignored when brandRoot empty", brandEmail, "", false},
		{"clean email passes", otherEmail, "discountblog.com", false},
		{"empty email short-circuits false", "", "discountblog.com", false},
		{"whitespace email normalizes", "  GLOBAL@EXAMPLE.COM  ", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := h.IsSuppressedForBrand(c.email, c.brandRoot); got != c.want {
				t.Errorf("IsSuppressedForBrand(%q, %q) = %v, want %v", c.email, c.brandRoot, got, c.want)
			}
		})
	}
}

// TestSuppressScoped_InsertsAndCaches verifies both sides of the write path:
// the DB row is inserted with the correct bind args AND the in-memory brandSet
// is populated, so the very next IsSuppressedForBrand call sees the entry.
func TestSuppressScoped_InsertsAndCaches(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	h := NewGlobalSuppressionHub(db, "00000000-0000-0000-0000-000000000001", "")

	email := "subscriber@example.com"
	brandRoot := "discountblog.com"

	mock.ExpectExec(`INSERT INTO mailing_domain_suppressions`).
		WithArgs(
			h.orgID, email, MD5Hash(email), brandRoot,
			"top_unsub", "track_handler",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	require.NoError(t, h.SuppressScoped(
		context.Background(), email, brandRoot, "top_unsub", "track_handler", "", "", "",
	))

	assert.True(t, h.IsSuppressedForBrand(email, brandRoot),
		"cache must reflect write immediately")
	assert.False(t, h.IsSuppressedForBrand(email, "quizfiesta.com"),
		"brand scope must NOT leak to other brands")
	assert.False(t, h.IsSuppressed(email),
		"brand-scoped write must NOT flip the global set")

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSuppressScoped_EmptyInputsAreNoOp guards against accidentally creating
// an all-match entry (brand_root="") or a blank-email row.
func TestSuppressScoped_EmptyInputsAreNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	h := NewGlobalSuppressionHub(db, "org-1", "")

	require.NoError(t, h.SuppressScoped(context.Background(), "", "discountblog.com", "r", "s", "", "", ""))
	require.NoError(t, h.SuppressScoped(context.Background(), "a@b.com", "", "r", "s", "", "", ""))
	require.NoError(t, h.SuppressScoped(context.Background(), "  ", "  ", "r", "s", "", "", ""))

	require.NoError(t, mock.ExpectationsWereMet(), "no DB writes expected for empty inputs")
}

// TestRemoveScoped_DeletesRowAndCache ensures the companion Remove is symmetric.
func TestRemoveScoped_DeletesRowAndCache(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	h := NewGlobalSuppressionHub(db, "org-1", "")
	email := "drop@example.com"
	brandRoot := "discountblog.com"
	h.brandSet[brandRoot] = map[string]bool{MD5Hash(email): true}

	mock.ExpectExec(`DELETE FROM mailing_domain_suppressions`).
		WithArgs(h.orgID, MD5Hash(email), brandRoot).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, h.RemoveScoped(context.Background(), email, brandRoot))

	assert.False(t, h.IsSuppressedForBrand(email, brandRoot),
		"cache must drop entry after remove")

	// Inner map should be cleaned up so we don't leak empty brand maps.
	_, stillThere := h.brandSet[brandRoot]
	assert.False(t, stillThere, "empty brand map should be deleted")

	require.NoError(t, mock.ExpectationsWereMet())
}

// A brand-scoped load cut short must return an error (so LoadFromDBWithRetry
// retries) and leave the previously loaded brand set intact — never merge the
// partial read and report success.
func TestLoadFromDB_TruncatedBrandLoadErrorsAndKeepsPriorSet(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	h := NewGlobalSuppressionHub(db, "org-1", "")
	prior := "prior@example.com"
	h.brandSet["discountblog.com"] = map[string]bool{MD5Hash(prior): true}

	mock.ExpectQuery(`SELECT email, md5_hash FROM mailing_global_suppressions`).
		WithArgs("org-1").
		WillReturnRows(sqlmock.NewRows([]string{"email", "md5_hash"}))
	mock.ExpectQuery(`SELECT brand_root, email_hash FROM mailing_domain_suppressions`).
		WithArgs("org-1").
		WillReturnRows(sqlmock.NewRows([]string{"brand_root", "email_hash"}).
			AddRow("historythinking.com", MD5Hash("partial@example.com")).
			AddRow("historythinking.com", MD5Hash("never@example.com")).
			RowError(1, errors.New("canceling statement due to statement timeout")))

	err = h.LoadFromDB(context.Background())
	require.Error(t, err, "truncated brand read must fail the load")
	assert.True(t, h.IsSuppressedForBrand(prior, "discountblog.com"), "previous brand set must survive")
	assert.False(t, h.IsSuppressedForBrand("partial@example.com", "historythinking.com"),
		"partial brand rows must not be merged")

	// A non-missing-table query error is also a failed load.
	mock.ExpectQuery(`SELECT email, md5_hash FROM mailing_global_suppressions`).
		WithArgs("org-1").
		WillReturnRows(sqlmock.NewRows([]string{"email", "md5_hash"}))
	mock.ExpectQuery(`SELECT brand_root, email_hash FROM mailing_domain_suppressions`).
		WithArgs("org-1").
		WillReturnError(errors.New("driver: bad connection"))
	require.Error(t, h.LoadFromDB(context.Background()))
	assert.True(t, h.IsSuppressedForBrand(prior, "discountblog.com"))

	// Missing table (42P01) stays a soft skip, as before.
	mock.ExpectQuery(`SELECT email, md5_hash FROM mailing_global_suppressions`).
		WithArgs("org-1").
		WillReturnRows(sqlmock.NewRows([]string{"email", "md5_hash"}))
	mock.ExpectQuery(`SELECT brand_root, email_hash FROM mailing_domain_suppressions`).
		WithArgs("org-1").
		WillReturnError(&pq.Error{Code: "42P01", Message: `relation "mailing_domain_suppressions" does not exist`})
	require.NoError(t, h.LoadFromDB(context.Background()))
	require.NoError(t, mock.ExpectationsWereMet())
}
