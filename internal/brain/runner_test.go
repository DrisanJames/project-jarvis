package brain

import (
	"encoding/json"
	"testing"
)

func rows(js string) any {
	var v any
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		panic(err)
	}
	return v
}

func TestCompareSQLRowsWithinTolerance(t *testing.T) {
	exp := rows(`[{"bucket":"microsoft","send":1000},{"bucket":"apple","send":250}]`)
	act := rows(`[{"bucket":"apple","send":"257"},{"bucket":"microsoft","send":1040,"extra":"ignored"}]`)
	pass, diffs := Compare("lake_sql", exp, act, 5)
	if !pass {
		t.Fatalf("expected pass within 5%%, diffs=%v", diffs)
	}
	pass, diffs = Compare("lake_sql", exp, act, 1)
	if pass || len(diffs) == 0 {
		t.Fatalf("expected fail at 1%%: %v", diffs)
	}
}

func TestCompareRowCountMismatchFails(t *testing.T) {
	exp := rows(`[{"a":1}]`)
	act := rows(`[{"a":1},{"a":2}]`)
	if pass, _ := Compare("pg_sql", exp, act, 0); pass {
		t.Fatal("row-count mismatch must fail")
	}
}

func TestCompareHTTPSubset(t *testing.T) {
	exp := rows(`{"status":200,"body":{"delivery":{"delivered":100}}}`)
	act := rows(`{"status":200,"body":{"delivery":{"delivered":102,"hard":3},"other":1}}`)
	if pass, d := Compare("http", exp, act, 3); !pass {
		t.Fatalf("subset within tolerance should pass: %v", d)
	}
	if pass, _ := Compare("http", exp, act, 0); pass {
		t.Fatal("102 vs 100 at zero tolerance must fail")
	}
	missing := rows(`{"status":200,"body":{}}`)
	if pass, d := Compare("http", exp, missing, 3); pass || len(d) == 0 {
		t.Fatal("missing key must fail with a diff")
	}
}

func TestAssertReadOnlySQL(t *testing.T) {
	ok := []string{"SELECT 1", "  with x as (select 1) select * from x", "(SELECT 1)"}
	bad := []string{"DELETE FROM t", "SELECT 1; DROP TABLE t", "UPDATE t SET a=1", "select * from t; select 2", "MSCK REPAIR TABLE x"}
	for _, q := range ok {
		if err := AssertReadOnlySQL(q); err != nil {
			t.Errorf("%q should be allowed: %v", q, err)
		}
	}
	for _, q := range bad {
		if err := AssertReadOnlySQL(q); err == nil {
			t.Errorf("%q should be refused", q)
		}
	}
}

func TestRedact(t *testing.T) {
	in := "dsn=postgres://ignite:pw@host/db password=abc token: xyz AKIAABCDEFGHIJKLMNOP fine"
	out := Redact(in)
	for _, leak := range []string{"pw@host", "password=abc", "token: xyz", "AKIAABCDEFGHIJKLMNOP"} {
		if contains([]string{out}, leak) || indexOf(out, leak) >= 0 {
			t.Errorf("leaked %q in %q", leak, out)
		}
	}
	if indexOf(out, "fine") < 0 {
		t.Errorf("over-redacted: %q", out)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRequiredVerifier(t *testing.T) {
	cases := map[string]string{"policy": "operator", "definition": "operator", "system_fact": "checker",
		"historical_finding": "review", "procedure": "review", "hypothesis": "review"}
	for ct, want := range cases {
		if got := RequiredVerifier(ct); got != want {
			t.Errorf("%s: got %s want %s", ct, got, want)
		}
	}
}

// A credential pasted into a claim body never reaches the store.
func TestClaimInputRedactsSecrets(t *testing.T) {
	in := ClaimInput{ClaimType: "system_fact", Title: "t", Body: "Authorization: sso-key ABC:DEF and AKIAABCDEFGHIJKLMNOP"}
	if err := in.validate(); err != nil {
		t.Fatal(err)
	}
	if indexOf(in.Body, "sso-key ABC") >= 0 || indexOf(in.Body, "AKIAABCDEFGHIJKLMNOP") >= 0 {
		t.Fatalf("secret survived validate: %q", in.Body)
	}
}
