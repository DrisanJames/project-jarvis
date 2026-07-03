package worker

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestPickWaveABVariant_DeterministicAndBalanced(t *testing.T) {
	variants := []waveABVariant{
		{CreativeID: uuid.MustParse("00000000-0000-0000-0000-00000000000a"), SplitPct: 50},
		{CreativeID: uuid.MustParse("00000000-0000-0000-0000-00000000000b"), SplitPct: 50},
	}
	counts := map[int]int{}
	for i := 0; i < 10000; i++ {
		sid := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("sub-%d", i)))
		first := pickWaveABVariant(sid, variants)
		if again := pickWaveABVariant(sid, variants); again != first {
			t.Fatalf("assignment not deterministic for %s", sid)
		}
		counts[first]++
	}
	for idx, n := range counts {
		if n < 4500 || n > 5500 {
			t.Fatalf("variant %d got %d/10000 — outside 45-55%% band", idx, n)
		}
	}
}

func TestPickWaveABVariant_RespectsSplits(t *testing.T) {
	variants := []waveABVariant{
		{CreativeID: uuid.MustParse("00000000-0000-0000-0000-00000000000a"), SplitPct: 90},
		{CreativeID: uuid.MustParse("00000000-0000-0000-0000-00000000000b"), SplitPct: 10},
	}
	b := 0
	for i := 0; i < 10000; i++ {
		sid := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("s%d", i)))
		if pickWaveABVariant(sid, variants) == 1 {
			b++
		}
	}
	if b < 500 || b > 1500 {
		t.Fatalf("10%% arm got %d/10000 — outside 5-15%% band", b)
	}
}

func TestLoadWaveABVariants_KillSwitchSkipsQuery(t *testing.T) {
	os.Setenv("DISABLE_WAVE_AB_SPLIT", "true")
	defer os.Unsetenv("DISABLE_WAVE_AB_SPLIT")
	db, mock, _ := sqlmock.New() // NO expectations: any query would fail the test
	defer db.Close()
	if got := loadWaveABVariants(context.Background(), db, uuid.New(), "w1", "", false); got != nil {
		t.Fatalf("kill switch must return nil, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB access under kill switch: %v", err)
	}
}

func TestLoadWaveABVariants_SingleVariantDisables(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	rows := sqlmock.NewRows([]string{"id", "html_content", "split_percent"}).
		AddRow(uuid.New().String(), "<html>only-one</html>", 100)
	mock.ExpectQuery(`FROM mailing_ab_variants`).WillReturnRows(rows)
	if got := loadWaveABVariants(context.Background(), db, uuid.New(), "w1", "", false); got != nil {
		t.Fatalf("single usable variant must disable the split, got %v", got)
	}
}

func TestLoadWaveABVariants_TwoVariantsSnapshotEach(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	vA, vB := uuid.New(), uuid.New()
	snapA, snapB := uuid.New(), uuid.New()
	rows := sqlmock.NewRows([]string{"id", "html_content", "split_percent"}).
		AddRow(vA.String(), "<html>A</html>", 50).
		AddRow(vB.String(), "<html>B</html>", 50)
	mock.ExpectQuery(`FROM mailing_ab_variants`).WillReturnRows(rows)
	// ensureContentSnapshot: hash-hit SELECT per variant
	mock.ExpectQuery(`SELECT id FROM mailing_content_snapshots`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(snapA.String()))
	mock.ExpectQuery(`SELECT id FROM mailing_content_snapshots`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(snapB.String()))

	got := loadWaveABVariants(context.Background(), db, uuid.New(), "w1", "", false)
	if len(got) != 2 {
		t.Fatalf("want 2 variants, got %d", len(got))
	}
	if got[0].SplitPct+got[1].SplitPct != 100 {
		t.Fatalf("splits must total 100")
	}
	seen := map[string]bool{got[0].SnapshotID.String(): true, got[1].SnapshotID.String(): true}
	if !seen[snapA.String()] || !seen[snapB.String()] {
		t.Fatalf("each variant must carry its own snapshot id")
	}
}
