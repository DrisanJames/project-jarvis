package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// decodeSubjectSecretForTest mirrors encode.py's decode_secret: collect the
// zero-width runes (U+200B=0, U+200C=1), regroup into bytes, and decode UTF-8.
// Used only to prove the Go encoder round-trips with the Python reference.
func decodeSubjectSecretForTest(encoded string) string {
	var bits strings.Builder
	for _, r := range encoded {
		switch r {
		case zwZero:
			bits.WriteByte('0')
		case zwOne:
			bits.WriteByte('1')
		}
	}
	b := bits.String()
	out := make([]byte, 0, len(b)/8)
	for i := 0; i+8 <= len(b); i += 8 {
		var v byte
		for j := 0; j < 8; j++ {
			v = v<<1 | (b[i+j] - '0')
		}
		out = append(out, v)
	}
	return string(out)
}

// TestEncodeSubjectSecret_ByteExact pins the exact byte sequence against a
// hand-computed reference so the Go port cannot silently drift from encode.py.
// Public "Ab", secret "A" (0x41 = 0100 0001):
//   result = 'A' + [zwZero zwOne zwZero zwZero  zwZero zwZero zwZero zwOne] + 'b'
func TestEncodeSubjectSecret_ByteExact(t *testing.T) {
	got := encodeSubjectSecret("Ab", "A")
	z, o := string(zwZero), string(zwOne)
	want := "A" + z + o + z + z + z + z + z + o + "b"
	if got != want {
		t.Fatalf("byte-exact mismatch\n got %v\nwant %v",
			[]rune(got), []rune(want))
	}
}

// TestEncodeSubjectSecret_RoundTrip proves the hidden payload survives a
// decode (the encode.py decode_secret contract) and the visible text is
// unchanged when the zero-width runes are stripped.
func TestEncodeSubjectSecret_RoundTrip(t *testing.T) {
	public := "Big savings inside 🎉"
	secret := "dom=qf;v=1"
	enc := encodeSubjectSecret(public, secret)

	if got := decodeSubjectSecretForTest(enc); got != secret {
		t.Errorf("decoded secret = %q, want %q", got, secret)
	}
	// Stripping the two zero-width runes must recover the original visible text.
	visible := strings.NewReplacer(string(zwZero), "", string(zwOne), "").Replace(enc)
	if visible != public {
		t.Errorf("visible text = %q, want %q", visible, public)
	}
	// The payload is exactly 8 zero-width runes per secret byte.
	var zwCount int
	for _, r := range enc {
		if r == zwZero || r == zwOne {
			zwCount++
		}
	}
	if want := 8 * len([]byte(secret)); zwCount != want {
		t.Errorf("zero-width rune count = %d, want %d", zwCount, want)
	}
}

// TestEncodeSubjectSecret_Empty proves a misconfiguration never mutates the
// subject (empty public or empty secret → unchanged).
func TestEncodeSubjectSecret_Empty(t *testing.T) {
	if got := encodeSubjectSecret("", "secret"); got != "" {
		t.Errorf("empty public: got %q, want empty", got)
	}
	if got := encodeSubjectSecret("Subject", ""); got != "Subject" {
		t.Errorf("empty secret: got %q, want unchanged", got)
	}
}

// TestEncodeSubjectSecret_MultibyteFirstRune confirms rune (not byte) slicing:
// a subject beginning with a multibyte rune keeps that rune intact and inserts
// the payload after it.
func TestEncodeSubjectSecret_MultibyteFirstRune(t *testing.T) {
	enc := encodeSubjectSecret("étoile", "x")
	if !strings.HasPrefix(enc, "é") {
		t.Fatalf("first rune corrupted: %q", enc)
	}
	// After stripping zero-width runes, the original must return byte-for-byte.
	visible := strings.NewReplacer(string(zwZero), "", string(zwOne), "").Replace(enc)
	if visible != "étoile" {
		t.Errorf("visible = %q, want étoile", visible)
	}
}

// --- Gate tests: maybeEncodeSubject must apply the payload ONLY when all three
// gates pass (kill switch off, recipient Yahoo, profile enabled + secret set).

// TestMaybeEncodeSubject_YahooEnabled: yahoo recipient + enabled profile with a
// secret → subject is encoded (carries the hidden payload).
func TestMaybeEncodeSubject_YahooEnabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	pool := &SendWorkerPool{db: db, subjectZWCache: make(map[string]subjectZWConfig)}

	pid := "yahoo-enabled-profile"
	mock.ExpectQuery("SELECT COALESCE\\(subject_zw_encode, FALSE\\), COALESCE\\(subject_zw_secret, ''\\)").
		WithArgs(pid).
		WillReturnRows(sqlmock.NewRows([]string{"subject_zw_encode", "subject_zw_secret"}).
			AddRow(true, "dom=qf"))

	got := pool.maybeEncodeSubject(context.Background(), "Hello", subjectZWGroup, pid)
	if got == "Hello" {
		t.Fatalf("expected encoded subject, got unchanged")
	}
	if decodeSubjectSecretForTest(got) != "dom=qf" {
		t.Errorf("hidden payload = %q, want dom=qf", decodeSubjectSecretForTest(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestMaybeEncodeSubject_NonYahooNoDBHit: a non-Yahoo recipient must short-circuit
// BEFORE any DB lookup (proves the Yahoo-only gate and avoids per-message queries
// on every other lane). No ExpectQuery is registered — a query would fail the mock.
func TestMaybeEncodeSubject_NonYahooNoDBHit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	pool := &SendWorkerPool{db: db, subjectZWCache: make(map[string]subjectZWConfig)}

	got := pool.maybeEncodeSubject(context.Background(), "Hello", "gmail", "any-profile")
	if got != "Hello" {
		t.Errorf("non-yahoo subject was mutated: %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB query on non-yahoo path: %v", err)
	}
}

// TestMaybeEncodeSubject_DisabledProfile: yahoo recipient but profile disabled →
// subject unchanged.
func TestMaybeEncodeSubject_DisabledProfile(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	pool := &SendWorkerPool{db: db, subjectZWCache: make(map[string]subjectZWConfig)}

	pid := "yahoo-disabled-profile"
	mock.ExpectQuery("SELECT COALESCE\\(subject_zw_encode, FALSE\\), COALESCE\\(subject_zw_secret, ''\\)").
		WithArgs(pid).
		WillReturnRows(sqlmock.NewRows([]string{"subject_zw_encode", "subject_zw_secret"}).
			AddRow(false, ""))

	got := pool.maybeEncodeSubject(context.Background(), "Hello", subjectZWGroup, pid)
	if got != "Hello" {
		t.Errorf("disabled profile mutated subject: %q", got)
	}
}

// TestMaybeEncodeSubject_KillSwitch: global kill switch forces a no-op even for a
// yahoo recipient on an enabled profile (no DB hit).
func TestMaybeEncodeSubject_KillSwitch(t *testing.T) {
	t.Setenv("DISABLE_SUBJECT_ZW_ENCODE", "true")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	pool := &SendWorkerPool{db: db, subjectZWCache: make(map[string]subjectZWConfig)}

	got := pool.maybeEncodeSubject(context.Background(), "Hello", subjectZWGroup, "any-profile")
	if got != "Hello" {
		t.Errorf("kill switch did not prevent encoding: %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB query when kill switch on: %v", err)
	}
}

// --- ApplySubjectZW: the proof / test-send entry point (package api uses it).

// TestApplySubjectZW_OverrideSecret: an inline override secret encodes for a
// yahoo recipient WITHOUT any DB lookup (nil db) — the test-send path that lets
// the operator verify the mechanism before enabling a domain.
func TestApplySubjectZW_OverrideSecret(t *testing.T) {
	res := ApplySubjectZW(context.Background(), nil, "Hello", subjectZWGroup, "any-profile", "test=1")
	if !res.Applied {
		t.Fatalf("override not applied: reason=%s", res.Reason)
	}
	if decodeSubjectSecretForTest(res.Subject) != "test=1" {
		t.Errorf("payload = %q, want test=1", decodeSubjectSecretForTest(res.Subject))
	}
}

// TestApplySubjectZW_OverrideStillYahooOnly: even with an override secret, a
// non-yahoo recipient is skipped — the Yahoo-only invariant holds in test mode.
func TestApplySubjectZW_OverrideStillYahooOnly(t *testing.T) {
	res := ApplySubjectZW(context.Background(), nil, "Hello", "gmail", "any-profile", "test=1")
	if res.Applied {
		t.Fatalf("override bypassed the Yahoo-only gate")
	}
	if res.Reason != "recipient_not_yahoo" {
		t.Errorf("reason = %q, want recipient_not_yahoo", res.Reason)
	}
	if res.Subject != "Hello" {
		t.Errorf("subject mutated: %q", res.Subject)
	}
}

// TestApplySubjectZW_FallbackToDBConfig: no override → the per-domain DB config
// is read; a disabled profile is skipped with reason domain_disabled.
func TestApplySubjectZW_FallbackToDBConfig(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	pid := "some-profile"
	mock.ExpectQuery("SELECT COALESCE\\(subject_zw_encode, FALSE\\), COALESCE\\(subject_zw_secret, ''\\)").
		WithArgs(pid).
		WillReturnRows(sqlmock.NewRows([]string{"subject_zw_encode", "subject_zw_secret"}).
			AddRow(false, ""))

	res := ApplySubjectZW(context.Background(), db, "Hello", subjectZWGroup, pid, "")
	if res.Applied || res.Reason != "domain_disabled" {
		t.Errorf("got applied=%v reason=%q, want applied=false reason=domain_disabled", res.Applied, res.Reason)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestApplySubjectZW_KillSwitch: global kill switch skips before any DB lookup
// even with an override secret.
func TestApplySubjectZW_KillSwitch(t *testing.T) {
	t.Setenv("DISABLE_SUBJECT_ZW_ENCODE", "true")
	res := ApplySubjectZW(context.Background(), nil, "Hello", subjectZWGroup, "p", "test=1")
	if res.Applied || res.Reason != "kill_switch" {
		t.Errorf("got applied=%v reason=%q, want applied=false reason=kill_switch", res.Applied, res.Reason)
	}
}
