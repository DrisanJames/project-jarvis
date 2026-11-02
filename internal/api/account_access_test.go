package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func expectDisabled(mock sqlmock.Sqlmock, email string, disabled bool) {
	mock.ExpectQuery("status = 'disabled'").WithArgs(email).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(disabled))
}

// The negative path through the middleware prod actually uses: a real session
// is refused once its user is disabled.
func TestAccessGate_DisabledUserDeniedOnTheLivePath(t *testing.T) {
	am, cookie, email := mintTestSession(t)
	s, mock, _ := newAccountTest(t, "")
	am.SetAccessGate(s.AccessAllowed)
	reached := false
	h := apiAuthMiddleware(am, "admin-key")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	call := func() int {
		req := httptest.NewRequest("GET", "/api/mailing/anything", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	expectDisabled(mock, email, false)
	if got := call(); got != http.StatusOK || !reached {
		t.Fatalf("active user: %d reached=%v", got, reached)
	}

	reached = false
	s.access.forget(email)
	expectDisabled(mock, email, true)
	if got := call(); got != http.StatusUnauthorized || reached {
		t.Fatalf("disabled user: %d reached=%v", got, reached)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAccessGate_Caches60s(t *testing.T) {
	s, mock, _ := newAccountTest(t, "")
	now := time.Unix(1700000000, 0)
	s.now = func() time.Time { return now }

	expectDisabled(mock, acctUser, true)
	if s.AccessAllowed(acctUser) {
		t.Fatal("disabled user allowed")
	}
	now = now.Add(59 * time.Second)
	if s.AccessAllowed(" USER@jamesventurescorp.com") { // no query expected, same entry
		t.Fatal("cache missed")
	}
	now = now.Add(2 * time.Second)
	expectDisabled(mock, acctUser, false)
	if !s.AccessAllowed(acctUser) {
		t.Fatal("re-enabled user still denied after the TTL")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAccessGate_FailsOpenOnDBError(t *testing.T) {
	s, mock, _ := newAccountTest(t, "")
	now := time.Unix(1700000000, 0)
	s.now = func() time.Time { return now }
	mock.ExpectQuery("status = 'disabled'").WillReturnError(errors.New("db down"))
	if !s.AccessAllowed(acctUser) {
		t.Fatal("a DB error must not lock people out")
	}
	if !s.AccessAllowed(acctUser) { // remembered, no second query
		t.Fatal("fail-open answer not remembered")
	}
	now = now.Add(accessFailOpenTTL + time.Second)
	expectDisabled(mock, acctUser, true)
	if s.AccessAllowed(acctUser) {
		t.Fatal("fail-open answer outlived its TTL")
	}
}

// Disabling on this instance takes effect at once, not after the cache.
func TestAccessGate_PatchForgetsCachedAnswer(t *testing.T) {
	target := uuid.New()
	s, mock, h := newAccountTest(t, acctAdmin)
	expectDisabled(mock, "t@jamesventurescorp.com", false)
	s.AccessAllowed("t@jamesventurescorp.com")

	expectRole(mock, acctAdmin, "admin", "active")
	expectPatch(mock, target, "user", "active")
	mock.ExpectQuery("UPDATE users SET role").
		WillReturnRows(acctUserRow(target, "t@jamesventurescorp.com", "user", "disabled"))
	mock.ExpectCommit()
	if rec := acctDo(h, "PATCH", "/account/users/"+target.String(), `{"status":"disabled"}`); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}

	expectDisabled(mock, "t@jamesventurescorp.com", true)
	if s.AccessAllowed("t@jamesventurescorp.com") {
		t.Fatal("disabled user still allowed from the cache")
	}
}
