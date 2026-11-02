package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestAccountMe_NoRowBecomesUser(t *testing.T) {
	_, mock, h := newAccountTest(t, "New.Person@JamesVenturesCorp.com")
	mock.ExpectQuery("INSERT INTO users").
		WithArgs(acctOrg, "new.person@jamesventurescorp.com", "Test Person", "g-1", "", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "name", "avatar_url", "role", "status", "last_login_at"}).
			AddRow(uuid.NewString(), "new.person@jamesventurescorp.com", "Test Person", nil, "user", "active", time.Unix(1700000000, 0)))
	rec := acctDo(h, "GET", "/account/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	var got accountProfile
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Role != "user" || got.CanManage {
		t.Fatalf("a new user must be a plain user, got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A forged header is not a session: identity comes from the session only.
func TestAccount_ForgedHeaderIsNotASession(t *testing.T) {
	_, _, h := newAccountTest(t, "")
	for _, p := range []string{"/account/me", "/account/company", "/account/subscription", "/account/users", "/account/keys"} {
		rec := acctDo(h, "GET", p, "", "X-User-Email", acctAdmin)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: forged header got %d", p, rec.Code)
		}
	}
}

func TestAccount_OrgScoping(t *testing.T) {
	otherOrg := uuid.New()
	_, mock, h := newAccountTest(t, acctAdmin)
	mock.ExpectQuery("FROM organizations WHERE id").WithArgs(otherOrg).
		WillReturnRows(sqlmock.NewRows([]string{"name", "slug", "plan", "status", "d", "m"}).AddRow("B", "b", "x", "active", 1, 2))
	if rec := acctDo(h, "GET", "/account/company", "", "X-Organization-ID", otherOrg.String()); rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	mock.ExpectQuery("FROM users WHERE organization_id").WithArgs(otherOrg).
		WillReturnRows(acctUserRow(uuid.New(), "x@jamesventurescorp.com", "user", "active"))
	if rec := acctDo(h, "GET", "/account/users", "", "X-Organization-ID", otherOrg.String()); rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	mock.ExpectQuery("FROM account_provider_keys").WithArgs(otherOrg).
		WillReturnRows(sqlmock.NewRows([]string{"key_name", "ciphertext", "last4", "updated_by", "updated_at"}))
	if rec := acctDo(h, "GET", "/account/keys", "", "X-Organization-ID", otherOrg.String()); rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Being owner in your own org means nothing in an org you merely name.
func TestAccount_RoleIsReadInTheRequestedOrg(t *testing.T) {
	otherOrg := uuid.New()
	_, mock, h := newAccountTest(t, acctAdmin)
	mock.ExpectQuery("FROM users WHERE organization_id").WithArgs(otherOrg, acctAdmin).
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}))
	rec := acctDo(h, "PUT", "/account/company", `{"name":"Takeover"}`, "X-Organization-ID", otherOrg.String())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

var acctWrites = []struct{ name, method, path, body string }{
	{"company", "PUT", "/account/company", `{"name":"X"}`},
	{"add user", "POST", "/account/users", `{"email":"a@jamesventurescorp.com","role":"user"}`},
	{"edit user", "PATCH", "/account/users/" + uuid.NewString(), `{"status":"disabled"}`},
	{"save key", "PUT", "/account/keys/ANTHROPIC_API_KEY", `{"value":"abcdefghijklmnopqrstuvwxyz"}`},
	{"delete key", "DELETE", "/account/keys/ANTHROPIC_API_KEY", ""},
}

func TestAccount_NonAdminRefusedOnEveryWrite(t *testing.T) {
	for _, role := range []string{"user", "viewer"} {
		for _, w := range acctWrites {
			t.Run(role+"/"+w.name, func(t *testing.T) {
				_, mock, h := newAccountTest(t, acctUser)
				expectRole(mock, acctUser, role, "active")
				if rec := acctDo(h, w.method, w.path, w.body); rec.Code != http.StatusForbidden {
					t.Fatalf("got %d: %s", rec.Code, rec.Body)
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestAccount_NoRowRefusedOnEveryWrite(t *testing.T) {
	for _, w := range acctWrites {
		t.Run(w.name, func(t *testing.T) {
			_, mock, h := newAccountTest(t, acctUser)
			mock.ExpectQuery("FROM users WHERE organization_id").WithArgs(acctOrg, acctUser).
				WillReturnRows(sqlmock.NewRows([]string{"role", "status"}))
			if rec := acctDo(h, w.method, w.path, w.body); rec.Code != http.StatusForbidden {
				t.Fatalf("got %d", rec.Code)
			}
		})
	}
}

// The DB check stands even when the gate let the request through.
func TestAccount_DisabledAdminRefused(t *testing.T) {
	_, mock, h := newAccountTest(t, acctAdmin)
	expectRole(mock, acctAdmin, "admin", "disabled")
	if rec := acctDo(h, "PUT", "/account/company", `{"name":"X"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestAccount_CompanyPutEditsNameOnly(t *testing.T) {
	_, mock, h := newAccountTest(t, acctAdmin)
	expectRole(mock, acctAdmin, "admin", "active")
	mock.ExpectExec("UPDATE organizations SET name").WithArgs("New Name", acctOrg).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM organizations WHERE id").WithArgs(acctOrg).
		WillReturnRows(sqlmock.NewRows([]string{"name", "slug", "plan", "status", "d", "m"}).AddRow("New Name", "ignite", "enterprise", "active", 1, 2))
	rec := acctDo(h, "PUT", "/account/company", `{"name":"  New Name ","plan":"free","slug":"evil"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"plan":"enterprise"`) {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAccount_Subscription(t *testing.T) {
	s, mock, h := newAccountTest(t, acctUser)
	s.now = func() time.Time { return time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC) } // still Oct 31 in Denver
	mock.ExpectQuery("FROM organizations WHERE id").WithArgs(acctOrg).
		WillReturnRows(sqlmock.NewRows([]string{"name", "slug", "plan", "status", "d", "m"}).AddRow("JVC", "ignite", "enterprise", "active", 1000000, 30000000))
	mock.ExpectQuery("FROM mailing_growth_daily").WithArgs("2026-10-01", "2026-10-31").
		WillReturnRows(sqlmock.NewRows([]string{"s", "c"}).AddRow(1234, time.Now()))
	rec := acctDo(h, "GET", "/account/subscription", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"billing_connected":false`, `"sent_this_month":1234`, `"month":"2026-10"`, `"plan":"enterprise"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	for _, invented := range []string{"price", "renew", "payment"} {
		if strings.Contains(body, invented) {
			t.Fatalf("invented field %q in %s", invented, body)
		}
	}
}

// A failed usage read is unknown, not zero.
func TestAccount_SubscriptionUsageFailureIsNull(t *testing.T) {
	_, mock, h := newAccountTest(t, acctUser)
	mock.ExpectQuery("FROM organizations WHERE id").
		WillReturnRows(sqlmock.NewRows([]string{"name", "slug", "plan", "status", "d", "m"}).AddRow("JVC", "ignite", "enterprise", "active", 1, 2))
	mock.ExpectQuery("FROM mailing_growth_daily").WillReturnError(errors.New("down"))
	rec := acctDo(h, "GET", "/account/subscription", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sent_this_month":null`) {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
}

func TestAccount_CreateUser(t *testing.T) {
	cases := []struct {
		name, body string
		code       int
	}{
		{"outside domain", `{"email":"a@gmail.com","role":"user"}`, 400},
		{"lookalike domain", `{"email":"a@jamesventurescorp.com.evil.io","role":"user"}`, 400},
		{"bad role", `{"email":"a@jamesventurescorp.com","role":"root"}`, 400},
		{"admin grants owner", `{"email":"a@jamesventurescorp.com","role":"owner"}`, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, mock, h := newAccountTest(t, acctAdmin)
			expectRole(mock, acctAdmin, "admin", "active")
			if rec := acctDo(h, "POST", "/account/users", c.body); rec.Code != c.code {
				t.Fatalf("got %d: %s", rec.Code, rec.Body)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("added", func(t *testing.T) {
		_, mock, h := newAccountTest(t, acctAdmin)
		expectRole(mock, acctAdmin, "admin", "active")
		mock.ExpectQuery("INSERT INTO users").WithArgs(acctOrg, "a@jamesventurescorp.com", "A", "viewer").
			WillReturnRows(acctUserRow(uuid.New(), "a@jamesventurescorp.com", "viewer", "active"))
		if rec := acctDo(h, "POST", "/account/users", `{"email":" A@jamesventurescorp.com","name":"A","role":"viewer"}`); rec.Code != http.StatusCreated {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
	})
	t.Run("already there", func(t *testing.T) {
		_, mock, h := newAccountTest(t, acctAdmin)
		expectRole(mock, acctAdmin, "admin", "active")
		mock.ExpectQuery("INSERT INTO users").WillReturnRows(sqlmock.NewRows([]string{"id"}))
		if rec := acctDo(h, "POST", "/account/users", `{"email":"a@jamesventurescorp.com"}`); rec.Code != http.StatusConflict {
			t.Fatalf("got %d", rec.Code)
		}
	})
}

// expectPatch sets up the locked reads of updateUser.
func expectPatch(mock sqlmock.Sqlmock, target uuid.UUID, curRole, curStatus string, owners ...uuid.UUID) {
	mock.ExpectBegin()
	rows := sqlmock.NewRows([]string{"id"})
	for _, o := range owners {
		rows.AddRow(o.String())
	}
	mock.ExpectQuery("SELECT id FROM users").WithArgs(acctOrg).WillReturnRows(rows)
	mock.ExpectQuery("FROM users WHERE id").WithArgs(target, acctOrg).
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow(curRole, curStatus))
}

func TestAccount_LastOwnerRefused(t *testing.T) {
	owner := uuid.New()
	for name, body := range map[string]string{
		"demote":  `{"role":"admin"}`,
		"disable": `{"status":"disabled"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, mock, h := newAccountTest(t, acctAdmin)
			expectRole(mock, acctAdmin, "owner", "active")
			expectPatch(mock, owner, "owner", "active", owner)
			mock.ExpectRollback()
			rec := acctDo(h, "PATCH", "/account/users/"+owner.String(), body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("got %d: %s", rec.Code, rec.Body)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("second owner exists", func(t *testing.T) {
		_, mock, h := newAccountTest(t, acctAdmin)
		expectRole(mock, acctAdmin, "owner", "active")
		other := uuid.New()
		expectPatch(mock, owner, "owner", "active", owner, other)
		mock.ExpectQuery("UPDATE users SET role").WithArgs("admin", "active", owner, acctOrg).
			WillReturnRows(acctUserRow(owner, "o@jamesventurescorp.com", "admin", "active"))
		mock.ExpectCommit()
		if rec := acctDo(h, "PATCH", "/account/users/"+owner.String(), `{"role":"admin"}`); rec.Code != http.StatusOK {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
	})
}

func TestAccount_AdminCannotTouchOwners(t *testing.T) {
	target := uuid.New()
	for name, tc := range map[string]struct{ curRole, body string }{
		"self promote": {"admin", `{"role":"owner"}`},
		"demote owner": {"owner", `{"role":"user"}`},
		"disable him":  {"owner", `{"status":"disabled"}`},
	} {
		t.Run(name, func(t *testing.T) {
			_, mock, h := newAccountTest(t, acctAdmin)
			expectRole(mock, acctAdmin, "admin", "active")
			expectPatch(mock, target, tc.curRole, "active", uuid.New(), uuid.New())
			mock.ExpectRollback()
			if rec := acctDo(h, "PATCH", "/account/users/"+target.String(), tc.body); rec.Code != http.StatusForbidden {
				t.Fatalf("got %d: %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestAccount_PatchValidates(t *testing.T) {
	for name, tc := range map[string]struct {
		path, body string
		code       int
	}{
		"bad role":   {"/account/users/" + uuid.NewString(), `{"role":"root"}`, 400},
		"bad status": {"/account/users/" + uuid.NewString(), `{"status":"invited"}`, 400},
		"empty":      {"/account/users/" + uuid.NewString(), `{}`, 400},
		"bad id":     {"/account/users/not-a-uuid", `{"role":"user"}`, 404},
	} {
		t.Run(name, func(t *testing.T) {
			_, mock, h := newAccountTest(t, acctAdmin)
			expectRole(mock, acctAdmin, "owner", "active")
			if rec := acctDo(h, "PATCH", tc.path, tc.body); rec.Code != tc.code {
				t.Fatalf("got %d", rec.Code)
			}
		})
	}
}

func TestAccount_PatchOtherOrgUserNotFound(t *testing.T) {
	target := uuid.New()
	_, mock, h := newAccountTest(t, acctAdmin)
	expectRole(mock, acctAdmin, "owner", "active")
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id FROM users").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM users WHERE id").WithArgs(target, acctOrg).
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}))
	mock.ExpectRollback()
	if rec := acctDo(h, "PATCH", "/account/users/"+target.String(), `{"role":"user"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}
