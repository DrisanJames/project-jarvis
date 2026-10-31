package analytics

import (
	"strings"
	"testing"
)

func cubeTestFilter() CubeFilter {
	return CubeFilter{From: "2026-10-30", To: "2026-10-30", Lanes: CubeLanes{
		PmtaA: []string{"db", "relay"}, PmtaB: []string{"ht"}, Colo2: []string{"ufo"},
	}}
}

func TestBuildCubeSQL_DeliveryAndEngagementComeFromOneSideEach(t *testing.T) {
	sql, err := buildCubeSQL(cubeTestFilter())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"source IN ('kumo', 'pmta', 'ses') AND event_type NOT IN ('open', 'click')",
		"source = 'app' AND event_type IN ('open', 'click')",
		"COUNT(DISTINCT e.event_uid)",
		"dt BETWEEN '2026-10-29' AND '2026-10-31'", // Denver day spans two UTC partitions
		localDtExpr + " BETWEEN '2026-10-30' AND '2026-10-30'",
		"dt BETWEEN '2026-10-26' AND '2026-10-31'", // campaign maps look back
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in:\n%s", want, sql)
		}
	}
}

func TestBuildCubeSQL_LanesFollowTheVmtaCodes(t *testing.T) {
	sql, err := buildCubeSQL(cubeTestFilter())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"WHEN source = 'ses' THEN 'ses'",
		"WHEN source = 'pmta' AND split_part(vmta, '-', 2) IN ('db', 'relay') THEN 'pmta_a'",
		"WHEN source = 'pmta' AND split_part(vmta, '-', 2) IN ('ht') THEN 'pmta_b'",
		"WHEN source = 'kumo' AND split_part(vmta, '-', 2) IN ('ufo') THEN 'colo2'",
		"WHEN source = 'kumo' THEN 'colo1'", // colo1 is the rest of kumo
		"WHEN source = 'pmta' THEN 'mta'",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestBuildCubeSQL_EmptyBoxScopesToNothing(t *testing.T) {
	f := cubeTestFilter()
	f.Lanes.Colo2 = nil
	sql, err := buildCubeSQL(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "'colo2'") {
		t.Errorf("empty box must not produce an IN () clause:\n%s", sql)
	}
}

func TestBuildCubeSQL_DimDropsDayAndBrandColumns(t *testing.T) {
	f := cubeTestFilter()
	f.Dim, f.Brand = "email_domain", "discountblog.com"
	sql, err := buildCubeSQL(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "AS dim_value") || !strings.Contains(sql, "GROUP BY 1, 2, 3, 4 ") {
		t.Errorf("dim shape wrong:\n%s", sql)
	}
	if strings.Contains(sql, "e.local_dt") {
		t.Errorf("dim cube must not group by day:\n%s", sql)
	}
	if !strings.Contains(sql, "= 'discountblog.com' OR ") || !strings.Contains(sql, "LIKE '%.discountblog.com'") {
		t.Errorf("brand must match apex and sub domains:\n%s", sql)
	}
}

func TestBuildCubeSQL_RejectsBadInput(t *testing.T) {
	cases := map[string]func(*CubeFilter){
		"sql in from":   func(f *CubeFilter) { f.From = "2026-10-30' OR 1=1" },
		"reversed":      func(f *CubeFilter) { f.From, f.To = "2026-10-31", "2026-10-30" },
		"unknown dim":   func(f *CubeFilter) { f.Dim = "email" },
		"quote brand":   func(f *CubeFilter) { f.Brand = "a'b.com" },
		"wildcard":      func(f *CubeFilter) { f.Brand = "%.com" },
		"bad vmta code": func(f *CubeFilter) { f.Lanes.PmtaA = []string{"db'"} },
	}
	for name, mut := range cases {
		f := cubeTestFilter()
		mut(&f)
		if _, err := buildCubeSQL(f); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseCubeRows(t *testing.T) {
	c, err := parseCubeRows(
		[]string{"local_dt", "lane", "isp", "et", "brand", "c"},
		[][]string{{"2026-10-30", "ses", "gmail", "delivered", "em.x.com", "12"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Rows[0]
	if r.LocalDt != "2026-10-30" || r.Lane != "ses" || r.ISP != "gmail" || r.Event != "delivered" || r.Brand != "em.x.com" || r.Count != 12 {
		t.Errorf("parsed wrong: %+v", r)
	}
	if c.Truncated {
		t.Error("should not be truncated")
	}
}

func TestParseCubeRows_FlagsTruncation(t *testing.T) {
	rows := make([][]string, maxCubeRows+1)
	for i := range rows {
		rows[i] = []string{"ses", "gmail", "open", "1"}
	}
	c, err := parseCubeRows([]string{"lane", "isp", "et", "c"}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Truncated || len(c.Rows) != maxCubeRows {
		t.Errorf("truncated=%v rows=%d", c.Truncated, len(c.Rows))
	}
}

func TestParseCubeRows_BadCountIsAnError(t *testing.T) {
	if _, err := parseCubeRows([]string{"lane", "isp", "et", "c"}, [][]string{{"ses", "gmail", "open", "x"}}); err == nil {
		t.Error("expected an error")
	}
}
