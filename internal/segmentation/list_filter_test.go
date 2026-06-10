package segmentation

import (
	"database/sql/driver"
	"reflect"
	"testing"
)

func TestEscapeLikePattern(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"50% off", `50\% off`},
		{"a_b", `a\_b`},
		{`back\slash`, `back\\slash`},
		{`%_\`, `\%\_\\`},
		{"", ""},
	}
	for _, tt := range tests {
		if got := escapeLikePattern(tt.in); got != tt.want {
			t.Errorf("escapeLikePattern(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// argValues flattens args (including pq.Array valuers) into comparable values.
func argValues(t *testing.T, args []interface{}) []driver.Value {
	t.Helper()
	out := make([]driver.Value, 0, len(args))
	for _, a := range args {
		if v, ok := a.(driver.Valuer); ok {
			val, err := v.Value()
			if err != nil {
				t.Fatalf("driver.Valuer error = %v", err)
			}
			out = append(out, val)
			continue
		}
		out = append(out, a)
	}
	return out
}

func TestBuildSegmentListFilterSQL(t *testing.T) {
	tests := []struct {
		name                   string
		filter                 SegmentListFilter
		next                   int
		includeCategoryFilters bool
		wantSQL                string
		wantArgs               []driver.Value
	}{
		{
			name:                   "zero filter emits nothing (legacy ListSegments)",
			filter:                 SegmentListFilter{},
			next:                   2,
			includeCategoryFilters: true,
			wantSQL:                "",
			wantArgs:               []driver.Value{},
		},
		{
			name:                   "name query is ILIKE-wrapped and escaped",
			filter:                 SegmentListFilter{NameQuery: `50%_off\deal`},
			next:                   2,
			includeCategoryFilters: true,
			wantSQL:                " AND ms.name ILIKE $2",
			wantArgs:               []driver.Value{`%50\%\_off\\deal%`},
		},
		{
			name:                   "status all is a no-op (same as absent)",
			filter:                 SegmentListFilter{Status: "all"},
			next:                   2,
			includeCategoryFilters: true,
			wantSQL:                "",
			wantArgs:               []driver.Value{},
		},
		{
			name:                   "explicit status becomes equality predicate",
			filter:                 SegmentListFilter{Status: "archived"},
			next:                   3,
			includeCategoryFilters: true,
			wantSQL:                " AND ms.status = $3",
			wantArgs:               []driver.Value{"archived"},
		},
		{
			name: "categories include and exclude with sequential placeholders",
			filter: SegmentListFilter{
				Categories:        []string{"funnel", "engaged-model"},
				ExcludeCategories: []string{"partner_wave_static"},
			},
			next:                   2,
			includeCategoryFilters: true,
			wantSQL: " AND COALESCE(ms.category, 'uncategorized') = ANY($2::text[])" +
				" AND COALESCE(ms.category, 'uncategorized') <> ALL($3::text[])",
			wantArgs: []driver.Value{`{"funnel","engaged-model"}`, `{"partner_wave_static"}`},
		},
		{
			name: "includeCategoryFilters=false drops category predicates but keeps q/status",
			filter: SegmentListFilter{
				NameQuery:         "warby",
				Status:            "active",
				Categories:        []string{"funnel"},
				ExcludeCategories: []string{"partner_wave_static"},
			},
			next:                   2,
			includeCategoryFilters: false,
			wantSQL:                " AND ms.name ILIKE $2 AND ms.status = $3",
			wantArgs:               []driver.Value{"%warby%", "active"},
		},
		{
			name: "all predicates combined number placeholders after list_id",
			filter: SegmentListFilter{
				NameQuery:         "eng",
				Status:            "active",
				Categories:        []string{"engagement_brand", "engagement_isp"},
				ExcludeCategories: []string{"legacy_snapshot"},
				Limit:             100, // Limit/Offset are NOT part of the WHERE fragment
				Offset:            50,
			},
			next:                   3, // $1=org, $2=list_id
			includeCategoryFilters: true,
			wantSQL: " AND ms.name ILIKE $3 AND ms.status = $4" +
				" AND COALESCE(ms.category, 'uncategorized') = ANY($5::text[])" +
				" AND COALESCE(ms.category, 'uncategorized') <> ALL($6::text[])",
			wantArgs: []driver.Value{"%eng%", "active",
				`{"engagement_brand","engagement_isp"}`, `{"legacy_snapshot"}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSQL, gotArgs := buildSegmentListFilterSQL(tt.filter, tt.next, tt.includeCategoryFilters)
			if gotSQL != tt.wantSQL {
				t.Fatalf("sql = %q, want %q", gotSQL, tt.wantSQL)
			}
			gotVals := argValues(t, gotArgs)
			if len(gotVals) == 0 && len(tt.wantArgs) == 0 {
				return
			}
			if !reflect.DeepEqual(gotVals, tt.wantArgs) {
				t.Fatalf("args = %#v, want %#v", gotVals, tt.wantArgs)
			}
		})
	}
}
