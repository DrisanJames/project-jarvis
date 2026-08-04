package segmentation

import (
	"strings"
	"testing"
)

// TestCompileV2SQL_Golden pins the exact SQL for a representative spec:
// two lists + opened-30d OR clicked-60d + isp eq + email_domain in +
// has_first_name. Any drift in clause order, placeholder numbering, or the
// index-predicate status filter fails loudly here.
func TestCompileV2SQL_Golden(t *testing.T) {
	raw := `{"v2":{
		"lists":["11111111-1111-1111-1111-111111111111","22222222-2222-2222-2222-222222222222"],
		"performance":{"opened_within_days":30,"clicked_within_days":60,"op":"or"},
		"refinements":[
			{"field":"isp","op":"eq","value":"Gmail"},
			{"field":"email_domain","op":"in","value":["gmail.com","googlemail.com"]},
			{"field":"has_first_name","op":"eq","value":true}
		]
	}}`
	c, err := ParseV2Criteria(raw)
	if err != nil {
		t.Fatalf("ParseV2Criteria: %v", err)
	}
	if c == nil {
		t.Fatal("ParseV2Criteria returned nil for a valid v2 payload")
	}

	sql, args, err := CompileV2SQL(c)
	if err != nil {
		t.Fatalf("CompileV2SQL: %v", err)
	}

	want := "SELECT s.id::text, s.email FROM mailing_subscribers s WHERE " +
		"s.list_id = ANY($1::uuid[]) AND " +
		"s.status IN ('active','confirmed') AND " +
		"(s.last_open_at >= NOW() - ($2 * INTERVAL '1 day') OR s.last_click_at >= NOW() - ($3 * INTERVAL '1 day')) AND " +
		"s.isp = $4 AND " +
		"split_part(LOWER(s.email), '@', 2) = ANY($5::text[]) AND " +
		"(s.first_name IS NOT NULL AND s.first_name <> '')"
	if sql != want {
		t.Errorf("golden SQL mismatch\n got: %s\nwant: %s", sql, want)
	}
	if len(args) != 5 {
		t.Fatalf("want 5 args, got %d: %v", len(args), args)
	}
	if args[1] != 30 || args[2] != 60 {
		t.Errorf("window args wrong: %v", args)
	}
	if args[3] != "gmail" { // isp value lowercased
		t.Errorf("isp arg not lowercased: %v", args[3])
	}
}

// TestCompileV2SQL_StatusRefinementReplacesDefault: an explicit status
// refinement must suppress the default active/confirmed filter (no
// contradictory double predicate).
func TestCompileV2SQL_StatusRefinementReplacesDefault(t *testing.T) {
	c, err := ParseV2Criteria(`{"v2":{"refinements":[{"field":"status","op":"eq","value":"unsubscribed"}]}}`)
	if err != nil || c == nil {
		t.Fatalf("parse: c=%v err=%v", c, err)
	}
	sql, _, err := CompileV2SQL(c)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if strings.Contains(sql, "('active','confirmed')") {
		t.Errorf("default status filter should be suppressed by explicit status refinement: %s", sql)
	}
	if !strings.Contains(sql, "s.status = $1") {
		t.Errorf("explicit status predicate missing: %s", sql)
	}
}

// TestParseV2Criteria_NonV2PassThrough: legacy shapes must return (nil, nil)
// so existing conditions never get hijacked into the v2 path.
func TestParseV2Criteria_NonV2PassThrough(t *testing.T) {
	for _, raw := range []string{
		"", "null", "[]",
		`[{"field":"email","operator":"contains","value":"@gmail.com"}]`, // legacy array
		`{"logic_operator":"and","conditions":[]}`,                       // ConditionGroupBuilder
		`{"lake_spec":{"event":"click","window_days":30}}`,               // lake segment
		`{"v2":null}`,
	} {
		c, err := ParseV2Criteria(raw)
		if c != nil || err != nil {
			t.Errorf("raw %q: want (nil,nil), got (%v,%v)", raw, c, err)
		}
	}
}

// TestParseV2Criteria_InvalidRejected: a present-but-broken v2 block must
// error (fail closed), never silently pass through.
func TestParseV2Criteria_InvalidRejected(t *testing.T) {
	for name, raw := range map[string]string{
		"empty object":       `{"v2":{}}`,
		"empty performance":  `{"v2":{"performance":{}}}`,
		"bad list uuid":      `{"v2":{"lists":["not-a-uuid"]}}`,
		"zero window":        `{"v2":{"performance":{"opened_within_days":0}}}`,
		"negative window":    `{"v2":{"performance":{"opened_within_days":-5}}}`,
		"bad perf op":        `{"v2":{"performance":{"opened_within_days":7,"op":"xor"}}}`,
		"unknown field":      `{"v2":{"refinements":[{"field":"shoe_size","op":"eq","value":"9"}]}}`,
		"bad op for field":   `{"v2":{"refinements":[{"field":"status","op":"contains","value":"act"}]}}`,
		"in without array":   `{"v2":{"refinements":[{"field":"isp","op":"in","value":"gmail"}]}}`,
		"bool field non-bool": `{"v2":{"refinements":[{"field":"has_first_name","op":"eq","value":"yes"}]}}`,
		"bad date":           `{"v2":{"refinements":[{"field":"created_after","op":"eq","value":"last tuesday"}]}}`,
		"empty string value":  `{"v2":{"refinements":[{"field":"isp","op":"eq","value":""}]}}`,
	} {
		c, err := ParseV2Criteria(raw)
		if err == nil {
			t.Errorf("%s: expected rejection, got criteria %+v", name, c)
		}
		if c != nil {
			t.Errorf("%s: invalid spec must return nil criteria", name)
		}
	}
}

// TestCompileV2CountSQL wraps the compiled SELECT in a LIMIT-capped COUNT.
func TestCompileV2CountSQL(t *testing.T) {
	c, err := ParseV2Criteria(`{"v2":{"performance":{"clicked_within_days":30}}}`)
	if err != nil || c == nil {
		t.Fatalf("parse: %v", err)
	}
	sql, args, err := CompileV2CountSQL(c)
	if err != nil {
		t.Fatalf("compile count: %v", err)
	}
	if !strings.HasPrefix(sql, "SELECT COUNT(*) FROM (SELECT s.id::text, s.email FROM mailing_subscribers s WHERE ") {
		t.Errorf("count wrapper malformed: %s", sql)
	}
	if !strings.HasSuffix(sql, "LIMIT 2000000) q") {
		t.Errorf("count LIMIT cap missing: %s", sql)
	}
	if len(args) != 1 || args[0] != 30 {
		t.Errorf("args: %v", args)
	}
}

// TestSummarizeV2 renders the human definition line.
func TestSummarizeV2(t *testing.T) {
	c, err := ParseV2Criteria(`{"v2":{
		"lists":["11111111-1111-1111-1111-111111111111"],
		"performance":{"opened_within_days":30,"clicked_within_days":60,"op":"or"},
		"refinements":[{"field":"isp","op":"eq","value":"gmail"}]
	}}`)
	if err != nil || c == nil {
		t.Fatalf("parse: %v", err)
	}
	got := SummarizeV2(c)
	want := "1 list · opened ≤30d or clicked ≤60d · isp = gmail"
	if got != want {
		t.Errorf("summary\n got: %s\nwant: %s", got, want)
	}
}
