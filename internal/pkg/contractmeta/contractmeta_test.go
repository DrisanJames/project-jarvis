package contractmeta_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
)

var (
	testKey  = []byte("test-contract-token-key-0123456789abcdef")
	issuedAt = time.Date(2026, 9, 3, 6, 0, 0, 0, time.UTC)
)

// ---------------------------------------------------------------------------
// Canonical
// ---------------------------------------------------------------------------

// Two bodies with the same content in a different key order must canonicalise
// to identical bytes — otherwise a token would depend on Go map iteration or on
// which struct the caller happened to hold.
func TestCanonical_IsOrderIndependent(t *testing.T) {
	a := json.RawMessage(`{"b":2,"a":1,"c":{"z":true,"y":[3,2,1]}}`)
	b := json.RawMessage(`{"c":{"y":[3,2,1],"z":true},"a":1,"b":2}`)

	ca := contractmeta.Canonical(a, "domain", "em.historythinking.com", 3)
	cb := contractmeta.Canonical(b, "domain", "em.historythinking.com", 3)
	if string(ca) != string(cb) {
		t.Fatalf("key order changed the canonical form:\n a=%s\n b=%s", ca, cb)
	}

	// A map and a struct carrying the same content also agree.
	type body struct {
		B int `json:"b"`
		A int `json:"a"`
	}
	m := map[string]any{"a": 1, "b": 2}
	if string(contractmeta.Canonical(body{B: 2, A: 1}, "k", "s", 1)) != string(contractmeta.Canonical(m, "k", "s", 1)) {
		t.Fatal("struct and map with equal content canonicalise differently")
	}

	// Repeated calls on the same Go map are stable (1000 iterations would catch
	// map-order leakage; encoding/json sorting plus our own sort make it exact).
	first := string(contractmeta.Canonical(m, "k", "s", 1))
	for i := 0; i < 200; i++ {
		if got := string(contractmeta.Canonical(m, "k", "s", 1)); got != first {
			t.Fatalf("unstable canonical form on iteration %d", i)
		}
	}
}

func TestCanonical_Shape(t *testing.T) {
	got := string(contractmeta.Canonical(map[string]any{"b": 2, "a": "x"}, "dispatch", "wcl_remail", 7))
	want := `{"body":{"a":"x","b":2},"kind":"dispatch","subject":"wcl_remail","version":7}`
	if got != want {
		t.Fatalf("canonical =\n %s\nwant\n %s", got, want)
	}
	if strings.ContainsAny(got, " \n\t") {
		t.Fatalf("canonical form carries whitespace: %q", got)
	}
}

// Numbers are reproduced verbatim from their JSON source. Decoding into `any`
// without json.Number yields float64, which cannot represent an integer above
// 2^53 and switches to exponent form at 1e21 — either would silently change a
// contract's token. Ordinary volume figures are unaffected either way; these
// values are the ones that actually diverge.
func TestCanonical_NumbersStayExact(t *testing.T) {
	// 2^53+1: float64 rounds this to ...992 and the digit is lost forever.
	body := json.RawMessage(`{"aol":17000,"big":9007199254740993,"huge":1e21}`)
	got := string(contractmeta.Canonical(body, "domain", "d", 1))

	for _, want := range []string{`"aol":17000`, `"big":9007199254740993`, `"huge":1e21`} {
		if !strings.Contains(got, want) {
			t.Fatalf("number mangled: want %s in %s", want, got)
		}
	}
	if strings.Contains(got, "9007199254740992") {
		t.Fatalf("integer lost precision through float64: %s", got)
	}
}

// Any change to kind, subject or version changes the canonical form, so a
// contract's token cannot be replayed onto a different subject or version.
func TestCanonical_EnvelopeIsPartOfTheDigest(t *testing.T) {
	base := contractmeta.Canonical(map[string]any{"a": 1}, "domain", "s1", 1)
	for _, v := range [][]byte{
		contractmeta.Canonical(map[string]any{"a": 1}, "dispatch", "s1", 1),
		contractmeta.Canonical(map[string]any{"a": 1}, "domain", "s2", 1),
		contractmeta.Canonical(map[string]any{"a": 1}, "domain", "s1", 2),
	} {
		if string(v) == string(base) {
			t.Fatalf("envelope field did not change the canonical form: %s", v)
		}
	}
}

// A body carrying its own "version" is not confused with the envelope's — the
// reason the union is an envelope and not a key merge.
func TestCanonical_BodyVersionDoesNotCollide(t *testing.T) {
	got := string(contractmeta.Canonical(map[string]any{"version": 99}, "domain", "s", 3))
	if !strings.Contains(got, `"body":{"version":99}`) || !strings.Contains(got, `"version":3}`) {
		t.Fatalf("body version collided with the envelope: %s", got)
	}
}

// An unmarshalable body can never produce a canonical form that a real body
// could also produce.
func TestCanonical_UnmarshalableBodyIsDistinct(t *testing.T) {
	got := string(contractmeta.Canonical(make(chan int), "domain", "s", 1))
	if !strings.Contains(got, `"_error"`) {
		t.Fatalf("expected an _error marker, got %s", got)
	}
	if got == string(contractmeta.Canonical(nil, "domain", "s", 1)) {
		t.Fatal("a broken body canonicalised the same as a nil body")
	}
}

// ---------------------------------------------------------------------------
// Issue / Verify
// ---------------------------------------------------------------------------

func TestIssueVerify_RoundTrip(t *testing.T) {
	canon := contractmeta.Canonical(map[string]any{"gmail": 0, "yahoo": 17000}, "domain", "em.historythinking.com", 3)
	tok := contractmeta.Issue(testKey, canon, issuedAt)

	if tok.Alg != contractmeta.AlgHMACSHA256 {
		t.Fatalf("alg = %q", tok.Alg)
	}
	if tok.IssuedBy != contractmeta.IssuerSystem {
		t.Fatalf("issued_by = %q", tok.IssuedBy)
	}
	if !tok.IssuedAt.Equal(issuedAt) || tok.IssuedAt.Location() != time.UTC {
		t.Fatalf("issued_at = %v, want %v in UTC", tok.IssuedAt, issuedAt)
	}
	if len(tok.Value) != 64 {
		t.Fatalf("value is %d hex chars, want 64 (sha256)", len(tok.Value))
	}
	if !tok.Issued() {
		t.Fatal("Issued() false on a real token")
	}
	if err := contractmeta.Verify(testKey, canon, tok); err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
	// Deterministic: the same body issues the same value.
	if contractmeta.Issue(testKey, canon, issuedAt.Add(time.Hour)).Value != tok.Value {
		t.Fatal("token value depends on issue time; it must depend only on key + body")
	}
}

// ONE field changed in the body -> the token no longer verifies.
func TestVerify_DetectsTamperedBody(t *testing.T) {
	body := map[string]any{"gmail": 0, "yahoo": 17000, "aol": 4900}
	canon := contractmeta.Canonical(body, "domain", "em.historythinking.com", 3)
	tok := contractmeta.Issue(testKey, canon, issuedAt)

	tampered := map[string]any{"gmail": 400, "yahoo": 17000, "aol": 4900} // gmail 0 -> 400
	tamperedCanon := contractmeta.Canonical(tampered, "domain", "em.historythinking.com", 3)

	if err := contractmeta.Verify(testKey, tamperedCanon, tok); !errors.Is(err, contractmeta.ErrTokenMismatch) {
		t.Fatalf("tampered body verified (or wrong error): %v", err)
	}
	// Negative control: the untouched body still verifies with the same token.
	if err := contractmeta.Verify(testKey, canon, tok); err != nil {
		t.Fatalf("untouched body must still verify: %v", err)
	}
}

func TestVerify_Refusals(t *testing.T) {
	canon := contractmeta.Canonical(map[string]any{"a": 1}, "domain", "s", 1)
	good := contractmeta.Issue(testKey, canon, issuedAt)

	cases := []struct {
		name string
		key  []byte
		tok  contractmeta.Token
		want error
	}{
		{"no key", nil, good, contractmeta.ErrNoKey},
		{"no token", testKey, contractmeta.Token{}, contractmeta.ErrNoToken},
		{"unsupported alg", testKey, contractmeta.Token{Alg: "md5", Value: good.Value}, contractmeta.ErrUnsupportedAlg},
		{"non-hex value", testKey, contractmeta.Token{Alg: contractmeta.AlgHMACSHA256, Value: "zzzz"}, contractmeta.ErrTokenMismatch},
		{"wrong key", []byte("a-different-key-0123456789abcdefghijk"), good, contractmeta.ErrTokenMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := contractmeta.Verify(tc.key, canon, tc.tok); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	// Negative control: the good token with the good key passes.
	if err := contractmeta.Verify(testKey, canon, good); err != nil {
		t.Fatalf("good token rejected: %v", err)
	}
}

// With no key, Issue returns the ZERO token — never a forgeable one computed
// under an empty key.
func TestIssue_WithoutKeyReturnsZeroToken(t *testing.T) {
	tok := contractmeta.Issue(nil, []byte("anything"), issuedAt)
	if tok.Issued() || tok.Value != "" || tok.Alg != "" {
		t.Fatalf("Issue produced a token without a key: %+v", tok)
	}
}

// ---------------------------------------------------------------------------
// KeyFromEnv
// ---------------------------------------------------------------------------

func TestKeyFromEnv(t *testing.T) {
	t.Run("unset fails closed", func(t *testing.T) {
		t.Setenv(contractmeta.KeyEnvVar, "")
		if _, err := contractmeta.KeyFromEnv(); !errors.Is(err, contractmeta.ErrNoKey) {
			t.Fatalf("got %v, want ErrNoKey", err)
		}
	})
	t.Run("too short fails closed", func(t *testing.T) {
		t.Setenv(contractmeta.KeyEnvVar, "short")
		if _, err := contractmeta.KeyFromEnv(); !errors.Is(err, contractmeta.ErrNoKey) {
			t.Fatalf("got %v, want ErrNoKey", err)
		}
	})
	t.Run("set returns the key", func(t *testing.T) {
		t.Setenv(contractmeta.KeyEnvVar, string(testKey))
		got, err := contractmeta.KeyFromEnv()
		if err != nil {
			t.Fatalf("KeyFromEnv: %v", err)
		}
		if string(got) != string(testKey) {
			t.Fatalf("key = %q", got)
		}
	})
	// There is no compiled-in fallback key: an unset env can never resolve.
	if os.Getenv(contractmeta.KeyEnvVar) != "" {
		t.Fatal("test leaked the key into the process environment")
	}
}

// ---------------------------------------------------------------------------
// Block JSONB round trip
// ---------------------------------------------------------------------------

func TestBlock_ScanValueRoundTrip(t *testing.T) {
	blk := contractmeta.Block{
		Refs: contractmeta.Refs{
			SendingDomainID: "aaaaaaaa-0000-0000-0000-000000000001",
			OwnedDomainID:   "historythinking.com",
			DatasetIDs:      []string{"d1", "d2"},
			SegmentIDs:      []string{"s1"},
		},
		Token: contractmeta.Issue(testKey, []byte("x"), issuedAt),
	}
	blk.StampIdentity("11111111-1111-1111-1111-111111111111", contractmeta.KindDomain, 15)
	blk.StampMutation(issuedAt, "operator", "chg-9", 14)

	v, err := blk.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	var back contractmeta.Block
	if err := back.Scan([]byte(v.(string))); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if back.ContractID != blk.ContractID || back.Kind != "domain" || back.Version != 15 {
		t.Fatalf("identity lost: %+v", back)
	}
	if back.Mutation.PriorVersion != 14 || back.Mutation.ChangeLedgerID != "chg-9" {
		t.Fatalf("mutation lost: %+v", back.Mutation)
	}
	if back.Refs.OwnedDomainID != "historythinking.com" || len(back.Refs.DatasetIDs) != 2 {
		t.Fatalf("refs lost: %+v", back.Refs)
	}
	if back.Token.Value != blk.Token.Value {
		t.Fatalf("token lost: %+v", back.Token)
	}

	// A NULL / empty / '{}' column scans to the zero block, which carries no
	// token and therefore fails verification — the right reading of "never issued".
	for _, src := range []any{nil, []byte(""), []byte("{}"), "{}"} {
		var z contractmeta.Block
		if err := z.Scan(src); err != nil {
			t.Fatalf("Scan(%v): %v", src, err)
		}
		if z.Token.Issued() {
			t.Fatalf("empty metadata produced an issued token: %+v", z)
		}
	}
	var bad contractmeta.Block
	if err := bad.Scan([]byte("not json")); err == nil {
		t.Fatal("Scan accepted non-JSON")
	}
	if err := bad.Scan(42); err == nil {
		t.Fatal("Scan accepted an int")
	}
}

// ---------------------------------------------------------------------------
// ResolveRefs
// ---------------------------------------------------------------------------

func TestResolveRefs_DomainAndDatasets(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text FROM mailing_sending_profiles WHERE sending_domain = $1 AND status = 'active'")).
		WithArgs("em.historythinking.com").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("aaaaaaaa-0000-0000-0000-000000000001"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT domain FROM mailing_owned_domains")).
		WithArgs("em.historythinking.com").
		WillReturnRows(sqlmock.NewRows([]string{"domain"}).AddRow("historythinking.com"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT slug, id::text FROM partner_datasets WHERE slug = ANY($1)")).
		WillReturnRows(sqlmock.NewRows([]string{"slug", "id"}).
			AddRow("wcl_abandon", "dddddddd-0000-0000-0000-000000000002").
			AddRow("globusa", "dddddddd-0000-0000-0000-000000000001"))

	refs, err := contractmeta.ResolveRefs(context.Background(), db, contractmeta.RefSpec{
		SendingDomain: "em.historythinking.com",
		DatasetSlugs:  []string{"wcl_abandon", "globusa", "wcl_abandon"}, // dupe collapsed
		SegmentIDs:    []string{"seg-2", "seg-1"},
	})
	if err != nil {
		t.Fatalf("ResolveRefs: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if refs.SendingDomainID != "aaaaaaaa-0000-0000-0000-000000000001" {
		t.Fatalf("sending_domain_id = %q", refs.SendingDomainID)
	}
	if refs.OwnedDomainID != "historythinking.com" {
		t.Fatalf("owned_domain_id = %q", refs.OwnedDomainID)
	}
	// Sorted, so the canonical form does not depend on the caller's order.
	if strings.Join(refs.DatasetIDs, ",") != "dddddddd-0000-0000-0000-000000000001,dddddddd-0000-0000-0000-000000000002" {
		t.Fatalf("dataset_ids = %v (want sorted, deduped)", refs.DatasetIDs)
	}
	if strings.Join(refs.SegmentIDs, ",") != "seg-1,seg-2" {
		t.Fatalf("segment_ids = %v", refs.SegmentIDs)
	}
}

// §1.5 rule 3: a missing sending profile fails the resolution — refs never
// resolve to an empty id.
func TestResolveRefs_MissingSendingProfileFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_sending_profiles")).
		WithArgs("em.doesnotexist.com").
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // no rows
	// The owned-domain query must never be reached.

	refs, err := contractmeta.ResolveRefs(context.Background(), db, contractmeta.RefSpec{
		SendingDomain: "em.doesnotexist.com",
	})
	var nf *contractmeta.ErrRefNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("expected *ErrRefNotFound, got %T: %v", err, err)
	}
	if nf.Table != "mailing_sending_profiles" || nf.Key != "em.doesnotexist.com" {
		t.Fatalf("ErrRefNotFound{%s,%s}", nf.Table, nf.Key)
	}
	if refs.SendingDomainID != "" || refs.OwnedDomainID != "" {
		t.Fatalf("partial refs returned on failure: %+v", refs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet (it must stop at the first miss): %v", err)
	}
}

func TestResolveRefs_MissingOwnedDomainAndDataset(t *testing.T) {
	t.Run("owned domain", func(t *testing.T) {
		db, mock, _ := sqlmock.New()
		defer db.Close()
		mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_sending_profiles")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("p1"))
		mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_owned_domains")).
			WillReturnRows(sqlmock.NewRows([]string{"domain"}))
		_, err := contractmeta.ResolveRefs(context.Background(), db, contractmeta.RefSpec{SendingDomain: "em.x.com"})
		var nf *contractmeta.ErrRefNotFound
		if !errors.As(err, &nf) || nf.Table != "mailing_owned_domains" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("dataset", func(t *testing.T) {
		db, mock, _ := sqlmock.New()
		defer db.Close()
		mock.ExpectQuery(regexp.QuoteMeta("FROM partner_datasets")).
			WillReturnRows(sqlmock.NewRows([]string{"slug", "id"}).AddRow("globusa", "d1"))
		_, err := contractmeta.ResolveDatasetIDs(context.Background(), db, []string{"globusa", "gone"})
		var nf *contractmeta.ErrRefNotFound
		if !errors.As(err, &nf) || nf.Table != "partner_datasets" || nf.Key != "gone" {
			t.Fatalf("got %v", err)
		}
	})
}

// An empty spec resolves to empty refs without touching the database.
func TestResolveRefs_EmptySpecIsANoop(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	refs, err := contractmeta.ResolveRefs(context.Background(), db, contractmeta.RefSpec{})
	if err != nil {
		t.Fatalf("ResolveRefs: %v", err)
	}
	if refs.SendingDomainID != "" || len(refs.DatasetIDs) != 0 {
		t.Fatalf("empty spec produced refs: %+v", refs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("empty spec queried the database: %v", err)
	}
}
