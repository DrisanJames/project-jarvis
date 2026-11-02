package api

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/auth"
)

const (
	acctAdmin = "admin@jamesventurescorp.com"
	acctUser  = "user@jamesventurescorp.com"
)

var acctOrg = uuid.MustParse(SingleTenantFallbackOrgID)

// newAccountTest builds a service on sqlmock with a stub session for email
// (empty = signed out), and a router carrying the real routes.
func newAccountTest(t *testing.T, email string) (*AccountService, sqlmock.Sqlmock, http.Handler) {
	t.Helper()
	t.Setenv("DEFAULT_ORG_ID", "")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewAccountService(db, nil)
	s.keyEnc = bytes.Repeat([]byte{7}, 32)
	s.applier = newProviderKeyApplier(db, s.keyEnc, acctOrg)
	s.emailAllowed = func(e string) bool { return strings.HasSuffix(e, "@jamesventurescorp.com") }
	s.session = func(*http.Request) *auth.Session {
		if email == "" {
			return nil
		}
		return &auth.Session{UserID: "g-1", Email: email, Name: "Test Person", CreatedAt: time.Unix(1700000000, 0)}
	}
	r := chi.NewRouter()
	s.RegisterRoutes(r)
	return s, mock, r
}

func acctDo(h http.Handler, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func expectRole(mock sqlmock.Sqlmock, email, role, status string) {
	mock.ExpectQuery("FROM users WHERE organization_id").
		WithArgs(acctOrg, email).
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow(role, status))
}

func captureAccountLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func acctUserRow(id uuid.UUID, email, role, status string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "email", "name", "avatar_url", "role", "status", "last_login_at", "created_at"}).
		AddRow(id.String(), email, nil, nil, role, status, nil, nil)
}
