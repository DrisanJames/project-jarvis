package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

const acctSecret = "sk-sentinel-DO-NOT-LEAK-0123456789-WXYZ"

var acctKey = bytes.Repeat([]byte{7}, 32)

func savedRow(t *testing.T, name, value, last4 string) *sqlmock.Rows {
	t.Helper()
	sealed, err := encryptProviderKey(acctKey, acctOrg, name, value)
	if err != nil {
		t.Fatal(err)
	}
	return sqlmock.NewRows([]string{"key_name", "ciphertext", "last4", "updated_by", "updated_at"}).
		AddRow(name, sealed, last4, acctAdmin, time.Now())
}

func TestProviderKeyCatalog_IsExactlyTheWhitelist(t *testing.T) {
	want := []string{"ANTHROPIC_API_KEY", "CONTENT_DESK_ANTHROPIC_API_KEY", "OPENAI_API_KEY",
		"EVERFLOW_API_KEY", "SLACK_BOT_TOKEN", "TWILIO_AUTH_TOKEN"}
	if len(providerKeyCatalog) != len(want) {
		t.Fatalf("catalog has %d keys", len(providerKeyCatalog))
	}
	for _, name := range want {
		spec, ok := providerKeyByName(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if spec.Applies != providerKeyRightAway && spec.Applies != providerKeyAfterDeploy {
			t.Fatalf("%s applies = %q", name, spec.Applies)
		}
		if spec.Label == "" || spec.InUseBy == "" {
			t.Fatalf("%s lacks plain-language text", name)
		}
	}
}

func TestAccountKeys_NonWhitelistedRefused(t *testing.T) {
	for _, name := range []string{"SESSION_SECRET", "TRACKING_SECRET", "ADMIN_API_KEY", "DATABASE_URL",
		"READ_REPLICA_URL", "GOOGLE_CLIENT_SECRET", "AWS_SES_SECRET_KEY", "PMTA_API_KEY", "anthropic_api_key"} {
		for _, method := range []string{"PUT", "DELETE"} {
			t.Run(method+" "+name, func(t *testing.T) {
				t.Setenv(name, "unchanged")
				_, mock, h := newAccountTest(t, acctAdmin)
				expectRole(mock, acctAdmin, "owner", "active")
				rec := acctDo(h, method, "/account/keys/"+name, `{"value":"`+acctSecret+`"}`)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("got %d", rec.Code)
				}
				if os.Getenv(name) != "unchanged" {
					t.Fatal("environment changed")
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestAccountKeys_ValueValidation(t *testing.T) {
	for name, value := range map[string]string{
		"short":       "abc",
		"too long":    strings.Repeat("a", providerKeyMaxLen+1),
		"inner space": "abcdefgh ijklmnopqrstuv",
		"newline":     "abcdefghijklmnopqrstuv\n",
		"tab":         "abcdefgh\tijklmnopqrstuv",
		"non ascii":   "abcdefghijklmnopqrstuvé",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", "before")
			_, mock, h := newAccountTest(t, acctAdmin)
			expectRole(mock, acctAdmin, "admin", "active")
			body, _ := json.Marshal(map[string]string{"value": value})
			rec := acctDo(h, "PUT", "/account/keys/OPENAI_API_KEY", string(body))
			if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), value) {
				t.Fatalf("got %d: %s", rec.Code, rec.Body)
			}
			if os.Getenv("OPENAI_API_KEY") != "before" {
				t.Fatal("environment changed")
			}
		})
	}
}

func TestAccountKeys_SaveNeverLeaksTheValue(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "deployed-value-1234567890")
	logs := captureAccountLog(t)
	_, mock, h := newAccountTest(t, acctAdmin)
	expectRole(mock, acctAdmin, "admin", "active")
	mock.ExpectExec("INSERT INTO account_provider_keys").
		WithArgs(acctOrg, "OPENAI_API_KEY", sqlmock.AnyArg(), "WXYZ", acctAdmin).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM account_provider_keys").WithArgs(acctOrg).
		WillReturnRows(savedRow(t, "OPENAI_API_KEY", acctSecret, "WXYZ"))

	rec := acctDo(h, "PUT", "/account/keys/OPENAI_API_KEY", `{"value":"`+acctSecret+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if os.Getenv("OPENAI_API_KEY") != acctSecret {
		t.Fatal("not applied on this instance")
	}
	for _, surface := range []string{rec.Body.String(), logs.String()} {
		if strings.Contains(surface, acctSecret) || strings.Contains(surface, "DO-NOT-LEAK") {
			t.Fatalf("value leaked: %s", surface)
		}
	}
	if !strings.Contains(logs.String(), "OPENAI_API_KEY saved by "+acctAdmin) {
		t.Fatalf("no audit line: %s", logs)
	}
	var row providerKeyRow
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row.Last4 != "WXYZ" || row.Source != providerKeySourceSaved {
		t.Fatalf("%+v", row)
	}
}

// Stores only ciphertext: the argument bound to the column is not the value.
func TestAccountKeys_StoredValueIsCiphertext(t *testing.T) {
	sealed, err := encryptProviderKey(acctKey, acctOrg, "OPENAI_API_KEY", acctSecret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(acctSecret)) {
		t.Fatal("plaintext in ciphertext")
	}
}

func TestAccountKeys_FailedSaveDoesNotApply(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "before")
	logs := captureAccountLog(t)
	_, mock, h := newAccountTest(t, acctAdmin)
	expectRole(mock, acctAdmin, "admin", "active")
	mock.ExpectExec("INSERT INTO account_provider_keys").WillReturnError(errors.New("db down"))
	rec := acctDo(h, "PUT", "/account/keys/OPENAI_API_KEY", `{"value":"`+acctSecret+`"}`)
	if rec.Code != http.StatusInternalServerError || os.Getenv("OPENAI_API_KEY") != "before" {
		t.Fatalf("got %d, env %q", rec.Code, os.Getenv("OPENAI_API_KEY"))
	}
	if strings.Contains(logs.String(), acctSecret) {
		t.Fatal("value in log")
	}
}

func TestAccountKeys_NoEncryptionKeyRefusesSave(t *testing.T) {
	s, mock, h := newAccountTest(t, acctAdmin)
	s.keyEnc = nil
	expectRole(mock, acctAdmin, "admin", "active")
	rec := acctDo(h, "PUT", "/account/keys/OPENAI_API_KEY", `{"value":"`+acctSecret+`"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestAccountKeys_OnlyPlatformOrgManagesKeys(t *testing.T) {
	other := uuid.New()
	t.Setenv("OPENAI_API_KEY", "before")
	_, mock, h := newAccountTest(t, acctAdmin)
	mock.ExpectQuery("FROM users WHERE organization_id").WithArgs(other, acctAdmin).
		WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow("owner", "active"))
	rec := acctDo(h, "PUT", "/account/keys/OPENAI_API_KEY", `{"value":"`+acctSecret+`"}`, "X-Organization-ID", other.String())
	if rec.Code != http.StatusForbidden || os.Getenv("OPENAI_API_KEY") != "before" {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestAccountKeys_ListShowsTailOnly(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", acctSecret)
	t.Setenv("EVERFLOW_API_KEY", "shortkey") // too short to show a tail
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("OPENAI_API_KEY", "ignored-deployment-value-9999")
	_, mock, h := newAccountTest(t, acctUser) // a plain user can read
	mock.ExpectQuery("FROM account_provider_keys").WithArgs(acctOrg).
		WillReturnRows(savedRow(t, "OPENAI_API_KEY", "saved-value-abcdefgh-1234", "1234"))
	rec := acctDo(h, "GET", "/account/keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), acctSecret) || strings.Contains(rec.Body.String(), "saved-value") {
		t.Fatalf("value leaked: %s", rec.Body)
	}
	var out struct {
		Keys []providerKeyRow `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]providerKeyRow{}
	for _, k := range out.Keys {
		got[k.Name] = k
	}
	if len(got) != len(providerKeyCatalog) {
		t.Fatalf("%d rows", len(got))
	}
	if k := got["ANTHROPIC_API_KEY"]; k.Source != providerKeySourceDeployment || k.Last4 != "WXYZ" || !k.Configured {
		t.Fatalf("%+v", k)
	}
	if k := got["OPENAI_API_KEY"]; k.Source != providerKeySourceSaved || k.Last4 != "1234" || k.UpdatedBy != acctAdmin {
		t.Fatalf("%+v", k)
	}
	if k := got["EVERFLOW_API_KEY"]; k.Last4 != "" || !k.Configured {
		t.Fatalf("%+v", k)
	}
	if k := got["SLACK_BOT_TOKEN"]; k.Configured {
		t.Fatalf("%+v", k)
	}
}

func TestAccountKeys_UnreadableRowIsReportedHonestly(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "deployment-value-abcd")
	_, mock, h := newAccountTest(t, acctUser)
	mock.ExpectQuery("FROM account_provider_keys").
		WillReturnRows(sqlmock.NewRows([]string{"key_name", "ciphertext", "last4", "updated_by", "updated_at"}).
			AddRow("OPENAI_API_KEY", []byte("garbage-not-a-valid-ciphertext-at-all"), "9999", acctAdmin, time.Now()))
	rec := acctDo(h, "GET", "/account/keys", "")
	var out struct {
		Keys []providerKeyRow `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range out.Keys {
		if k.Name == "OPENAI_API_KEY" {
			if k.Source != providerKeySourceDeployment || k.Last4 != "abcd" || k.Warning == "" {
				t.Fatalf("%+v", k)
			}
		}
	}
}

func TestAccountKeys_DeleteRevertsAtNextBoot(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "deployment-value-aaaa")
	prev := deploymentKeys.Load()
	deploymentKeys.Store(nil)
	captureDeploymentKeys()
	t.Cleanup(func() { deploymentKeys.Store(prev) })
	os.Setenv("OPENAI_API_KEY", acctSecret) // the saved value, running

	_, mock, h := newAccountTest(t, acctAdmin)
	expectRole(mock, acctAdmin, "admin", "active")
	mock.ExpectExec("DELETE FROM account_provider_keys").WithArgs(acctOrg, "OPENAI_API_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM account_provider_keys").
		WillReturnRows(sqlmock.NewRows([]string{"key_name", "ciphertext", "last4", "updated_by", "updated_at"}))
	rec := acctDo(h, "DELETE", "/account/keys/OPENAI_API_KEY", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var row providerKeyRow
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row.Source != providerKeySourceDeployment || !row.RevertsOnRestart {
		t.Fatalf("%+v", row)
	}
	if os.Getenv("OPENAI_API_KEY") != acctSecret {
		t.Fatal("delete must leave the running value until the next boot")
	}
}

func TestAccountKeys_DeleteNothingSaved(t *testing.T) {
	_, mock, h := newAccountTest(t, acctAdmin)
	expectRole(mock, acctAdmin, "admin", "active")
	mock.ExpectExec("DELETE FROM account_provider_keys").WillReturnResult(sqlmock.NewResult(0, 0))
	if rec := acctDo(h, "DELETE", "/account/keys/OPENAI_API_KEY", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestProviderKeyCrypto_RoundTrip(t *testing.T) {
	sealed, err := encryptProviderKey(acctKey, acctOrg, "SLACK_BOT_TOKEN", acctSecret)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decryptProviderKey(acctKey, acctOrg, "SLACK_BOT_TOKEN", sealed)
	if err != nil || got != acctSecret {
		t.Fatalf("%q %v", got, err)
	}
	again, _ := encryptProviderKey(acctKey, acctOrg, "SLACK_BOT_TOKEN", acctSecret)
	if bytes.Equal(sealed, again) {
		t.Fatal("nonce reused")
	}
}

func TestProviderKeyCrypto_TamperFails(t *testing.T) {
	sealed, err := encryptProviderKey(acctKey, acctOrg, "SLACK_BOT_TOKEN", acctSecret)
	if err != nil {
		t.Fatal(err)
	}
	for i := range []int{0, 1} {
		flipped := append([]byte(nil), sealed...)
		flipped[len(flipped)-1-i*20] ^= 0x01
		if _, err := decryptProviderKey(acctKey, acctOrg, "SLACK_BOT_TOKEN", flipped); err == nil {
			t.Fatal("flipped byte accepted")
		}
	}
	if _, err := decryptProviderKey(acctKey, acctOrg, "OPENAI_API_KEY", sealed); err == nil {
		t.Fatal("ciphertext moved to another key accepted")
	}
	if _, err := decryptProviderKey(acctKey, uuid.New(), "SLACK_BOT_TOKEN", sealed); err == nil {
		t.Fatal("ciphertext moved to another org accepted")
	}
	if _, err := decryptProviderKey(bytes.Repeat([]byte{8}, 32), acctOrg, "SLACK_BOT_TOKEN", sealed); err == nil {
		t.Fatal("wrong key accepted")
	}
	if _, err := decryptProviderKey(acctKey, acctOrg, "SLACK_BOT_TOKEN", []byte("x")); err == nil {
		t.Fatal("short ciphertext accepted")
	}
}

func TestProviderKeyRefresh_ConvergesAndSkipsBadRows(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "old-old-old-old-old")
	t.Setenv("SLACK_BOT_TOKEN", "keep-this-deployed-token")
	t.Setenv("SESSION_SECRET", "never-touched")
	logs := captureAccountLog(t)
	s, mock, _ := newAccountTest(t, "")
	good, _ := encryptProviderKey(acctKey, acctOrg, "OPENAI_API_KEY", acctSecret)
	smuggled, _ := encryptProviderKey(acctKey, acctOrg, "SESSION_SECRET", acctSecret)
	mock.ExpectQuery("FROM account_provider_keys").WithArgs(acctOrg).
		WillReturnRows(sqlmock.NewRows([]string{"key_name", "ciphertext"}).
			AddRow("SLACK_BOT_TOKEN", []byte("corrupt-corrupt-corrupt-corrupt-corrupt")).
			AddRow("SESSION_SECRET", smuggled).
			AddRow("OPENAI_API_KEY", good))
	s.refreshProviderKeys(context.Background())

	if os.Getenv("OPENAI_API_KEY") != acctSecret {
		t.Fatal("newest value not applied")
	}
	if os.Getenv("SLACK_BOT_TOKEN") != "keep-this-deployed-token" {
		t.Fatal("bad row touched the environment")
	}
	if os.Getenv("SESSION_SECRET") != "never-touched" {
		t.Fatal("a row off the whitelist reached the environment")
	}
	if strings.Contains(logs.String(), acctSecret) {
		t.Fatal("value in log")
	}
	if !strings.Contains(logs.String(), "SLACK_BOT_TOKEN") {
		t.Fatalf("bad row not logged: %s", logs)
	}

	// Same bad row next tick: no repeat log line.
	before := strings.Count(logs.String(), "can't be decrypted")
	mock.ExpectQuery("FROM account_provider_keys").
		WillReturnRows(sqlmock.NewRows([]string{"key_name", "ciphertext"}).
			AddRow("SLACK_BOT_TOKEN", []byte("corrupt-corrupt-corrupt-corrupt-corrupt")))
	s.refreshProviderKeys(context.Background())
	if strings.Count(logs.String(), "can't be decrypted") != before {
		t.Fatal("repeat warning every tick")
	}
}

func TestBootLoader_FailurePathsKeepEnvValues(t *testing.T) {
	const deployed = "deployed-value-abcdefgh"
	t.Setenv("OPENAI_API_KEY", deployed)
	logs := captureAccountLog(t)
	ctx := context.Background()

	if n := loadSavedProviderKeys(ctx, nil, acctOrg, func() (*sql.DB, error) {
		t.Fatal("must not open a DB without a key")
		return nil, nil
	}); n != 0 {
		t.Fatal("applied without a key")
	}

	// No DB. The URL parse error text carries the password; the log must not.
	parseErr := &url.Error{Op: "parse", URL: "postgres://u:hunter2-password@h/db", Err: errors.New("bad")}
	if n := loadSavedProviderKeys(ctx, acctKey, acctOrg, func() (*sql.DB, error) { return nil, parseErr }); n != 0 {
		t.Fatal("applied without a DB")
	}
	if strings.Contains(logs.String(), "hunter2") {
		t.Fatalf("password in log: %s", logs)
	}

	// Table missing (first boot) or DB refused the query.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("FROM account_provider_keys").WillReturnError(errors.New(`relation "account_provider_keys" does not exist`))
	mock.ExpectClose()
	if n := loadSavedProviderKeys(ctx, acctKey, acctOrg, func() (*sql.DB, error) { return db, nil }); n != 0 {
		t.Fatal("applied on a query failure")
	}

	// Rotated secret: decrypt fails, boot goes on.
	db2, mock2, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := encryptProviderKey(bytes.Repeat([]byte{9}, 32), acctOrg, "OPENAI_API_KEY", acctSecret)
	mock2.ExpectQuery("FROM account_provider_keys").
		WillReturnRows(sqlmock.NewRows([]string{"key_name", "ciphertext"}).AddRow("OPENAI_API_KEY", sealed))
	mock2.ExpectClose()
	if n := loadSavedProviderKeys(ctx, acctKey, acctOrg, func() (*sql.DB, error) { return db2, nil }); n != 0 {
		t.Fatal("applied an undecryptable row")
	}
	if os.Getenv("OPENAI_API_KEY") != deployed {
		t.Fatal("env changed on a failure path")
	}
}

func TestBootLoader_AppliesSavedValue(t *testing.T) {
	t.Setenv("EVERFLOW_API_KEY", "deployed-value-abcdefgh")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := encryptProviderKey(acctKey, acctOrg, "EVERFLOW_API_KEY", acctSecret)
	mock.ExpectQuery("FROM account_provider_keys").WithArgs(acctOrg).
		WillReturnRows(sqlmock.NewRows([]string{"key_name", "ciphertext"}).AddRow("EVERFLOW_API_KEY", sealed))
	mock.ExpectClose()
	if n := loadSavedProviderKeys(context.Background(), acctKey, acctOrg, func() (*sql.DB, error) { return db, nil }); n != 1 {
		t.Fatalf("applied %d", n)
	}
	if os.Getenv("EVERFLOW_API_KEY") != acctSecret {
		t.Fatal("saved value not in the environment")
	}
}

// Saves, background refreshes, list reads and gate checks all at once. The
// point is -race: every shared piece of state must be guarded.
func TestAccountKeys_ConcurrentSaveRefreshAndRead(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "deployed-value-abcdefgh")
	s, mock, h := newAccountTest(t, acctAdmin)
	mock.MatchExpectationsInOrder(false)
	const rounds = 20
	sealed, _ := encryptProviderKey(acctKey, acctOrg, "OPENAI_API_KEY", acctSecret)
	for i := 0; i < rounds; i++ {
		mock.ExpectQuery("SELECT key_name, ciphertext FROM account_provider_keys").
			WillReturnRows(sqlmock.NewRows([]string{"key_name", "ciphertext"}).AddRow("OPENAI_API_KEY", sealed))
		mock.ExpectQuery("SELECT key_name, ciphertext, last4").
			WillReturnRows(savedRow(t, "OPENAI_API_KEY", acctSecret, "WXYZ"))
		mock.ExpectQuery("status = 'disabled'").
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	}
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); s.refreshProviderKeys(context.Background()) }()
		go func() { defer wg.Done(); acctDo(h, "GET", "/account/keys", "") }()
		go func(i int) { defer wg.Done(); s.AccessAllowed(fmt.Sprintf("u%d@jamesventurescorp.com", i)) }(i)
	}
	wg.Wait()
}
