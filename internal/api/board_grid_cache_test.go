package api

// Board-grid success-cache fixtures (2026-08-22, IO-starved RDS):
//   - a fresh cached grid (≤60s) is served WITHOUT re-querying (zero SQL on
//     the second call — ordered sqlmock proves it);
//   - a loadCells failure with a warm cache (≤10 min) serves the last good
//     grid 200 with stale_as_of stamped — never an error while a usable
//     grid exists;
//   - a loadCells failure with NO cache keeps respondLoadError's contract:
//     the friendly 15s-timeout 503 for a deadline, 500 otherwise;
//   - the cache is bounded (boardGridCacheMaxEntries, oldest evicted).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newBoardGridServiceWithMock(t *testing.T) (*BoardGridService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewBoardGridService(db), mock
}

func boardGridCellColumns() []string {
	return []string{"brand_code", "brand_label", "sending_domain", "brand_root", "slot",
		"campaign_id", "name", "offer_id", "offer_name", "subject", "status", "recipients",
		"pending_finalize", "failure_reason", "isp_plans_json", "stuck_finalize",
		"preheader", "from_name", "from_email", "creative_len"}
}

func expectBoardGridCells(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WillReturnRows(sqlmock.NewRows(boardGridCellColumns()).
			AddRow("DB", "Discount Blog", "em.discountblog.com", "discountblog.com", "01:01",
				"c-1", "08222026 - DB - OFR-CLK", "of-1", "Optima Tax Relief", "Settle for less", "scheduled", 1200, false, "", "[]", false,
				"Your refund, itemized", "Optima Tax", "hello@em.discountblog.com", 8515))
}

func getBoardGrid(t *testing.T, s *BoardGridService, date string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/mailing/board-grid?date="+date, nil)
	rec := httptest.NewRecorder()
	s.HandleGetGrid(rec, req)
	return rec
}

// expireBoardGridCache back-dates every cached entry by age so the fresh TTL
// lapses while the stale window still holds (or not, per age).
func expireBoardGridCache(t *testing.T, age time.Duration) {
	t.Helper()
	boardGridCache.Lock()
	defer boardGridCache.Unlock()
	if len(boardGridCache.entries) == 0 {
		t.Fatal("expected a cached grid to expire")
	}
	for k, e := range boardGridCache.entries {
		e.computedAt = time.Now().Add(-age)
		boardGridCache.entries[k] = e
	}
}

// TestBoardGridCache_FreshCacheServesWithoutQuery: within the 60s TTL the
// second call issues ZERO SQL and returns the same grid, unstamped.
func TestBoardGridCache_FreshCacheServesWithoutQuery(t *testing.T) {
	resetBoardGridCacheForTest()
	t.Cleanup(resetBoardGridCacheForTest)
	s, mock := newBoardGridServiceWithMock(t)

	expectBoardGridCells(mock)
	rec1 := getBoardGrid(t, s, "2026-08-22")
	if rec1.Code != http.StatusOK {
		t.Fatalf("call 1 got %d: %s", rec1.Code, rec1.Body.String())
	}
	// Call 2: no expectations queued — any query would error the load and,
	// with a fresh cache in place, must not even be attempted.
	rec2 := getBoardGrid(t, s, "2026-08-22")
	if rec2.Code != http.StatusOK {
		t.Fatalf("call 2 got %d: %s", rec2.Code, rec2.Body.String())
	}
	var g1, g2 BoardGrid
	if err := json.Unmarshal(rec1.Body.Bytes(), &g1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &g2); err != nil {
		t.Fatal(err)
	}
	if len(g2.Cells) != 1 || g2.Cells[0].Name != g1.Cells[0].Name {
		t.Fatalf("cached grid must match the live one: %+v vs %+v", g2.Cells, g1.Cells)
	}
	if g1.StaleAsOf != nil || g2.StaleAsOf != nil {
		t.Fatal("a live or fresh-cache read must NOT carry stale_as_of")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the fresh cache must serve without re-querying: %v", err)
	}
}

// TestBoardGridCache_StaleServedOnLoadFailure: fresh TTL lapsed, load fails →
// the last good grid (≤10 min) is served 200 with stale_as_of set.
func TestBoardGridCache_StaleServedOnLoadFailure(t *testing.T) {
	resetBoardGridCacheForTest()
	t.Cleanup(resetBoardGridCacheForTest)
	s, mock := newBoardGridServiceWithMock(t)

	expectBoardGridCells(mock)
	if rec := getBoardGrid(t, s, "2026-08-22"); rec.Code != http.StatusOK {
		t.Fatalf("prime call got %d: %s", rec.Code, rec.Body.String())
	}
	expireBoardGridCache(t, 2*time.Minute) // past 60s fresh, inside 10 min stale

	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WillReturnError(fmt.Errorf("board grid query: %w", context.DeadlineExceeded))
	rec := getBoardGrid(t, s, "2026-08-22")
	if rec.Code != http.StatusOK {
		t.Fatalf("a warm cache must absorb the failure: got %d: %s", rec.Code, rec.Body.String())
	}
	var g BoardGrid
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	if g.StaleAsOf == nil {
		t.Fatal("a stale serve must be STAMPED via stale_as_of, never silent")
	}
	if age := time.Since(*g.StaleAsOf); age < time.Minute || age > 5*time.Minute {
		t.Fatalf("stale_as_of must be the cached compute instant (~2m old), got age %v", age)
	}
	if len(g.Cells) != 1 || g.Cells[0].Name != "08222026 - DB - OFR-CLK" {
		t.Fatalf("the cached cells must ride through: %+v", g.Cells)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestBoardGridCache_ColdCacheKeepsErrorContract: with NO cache the original
// respondLoadError contract holds — the friendly 15s-timeout 503 on a
// deadline, a generic 500 otherwise. A cache too old for the stale window
// (>10 min) behaves like no cache.
func TestBoardGridCache_ColdCacheKeepsErrorContract(t *testing.T) {
	resetBoardGridCacheForTest()
	t.Cleanup(resetBoardGridCacheForTest)

	t.Run("deadline → friendly 503", func(t *testing.T) {
		s, mock := newBoardGridServiceWithMock(t)
		mock.ExpectQuery(`FROM mailing_campaigns c`).
			WillReturnError(fmt.Errorf("board grid query: %w", context.DeadlineExceeded))
		rec := getBoardGrid(t, s, "2026-08-22")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("got %d, want 503", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "timed out after 15s") {
			t.Fatalf("the friendly timeout text must survive: %s", rec.Body.String())
		}
	})

	t.Run("generic error → 500", func(t *testing.T) {
		s, mock := newBoardGridServiceWithMock(t)
		mock.ExpectQuery(`FROM mailing_campaigns c`).WillReturnError(fmt.Errorf("boom"))
		rec := getBoardGrid(t, s, "2026-08-22")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", rec.Code)
		}
	})

	t.Run("cache older than the stale window → 503", func(t *testing.T) {
		s, mock := newBoardGridServiceWithMock(t)
		expectBoardGridCells(mock)
		if rec := getBoardGrid(t, s, "2026-08-22"); rec.Code != http.StatusOK {
			t.Fatalf("prime call got %d", rec.Code)
		}
		expireBoardGridCache(t, 11*time.Minute)
		mock.ExpectQuery(`FROM mailing_campaigns c`).
			WillReturnError(fmt.Errorf("board grid query: %w", context.DeadlineExceeded))
		rec := getBoardGrid(t, s, "2026-08-22")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("an 11-min-old cache is past the stale window: got %d", rec.Code)
		}
	})
}

// TestBoardGridCache_Bounded: the map never exceeds boardGridCacheMaxEntries;
// the oldest entry is the one evicted.
func TestBoardGridCache_Bounded(t *testing.T) {
	resetBoardGridCacheForTest()
	t.Cleanup(resetBoardGridCacheForTest)
	base := time.Now()
	total := boardGridCacheMaxEntries + 8
	for i := 0; i < total; i++ {
		key := "org|2026-08-" + strconv.Itoa(i)
		boardGridCachePut(key, BoardGrid{Date: key})
		// Back-date each entry distinctly (older for smaller i, all in the
		// PAST) so "oldest" is well-defined and eviction order observable.
		boardGridCache.Lock()
		if e, ok := boardGridCache.entries[key]; ok {
			e.computedAt = base.Add(-time.Duration(total-i) * time.Second)
			boardGridCache.entries[key] = e
		}
		boardGridCache.Unlock()
	}
	boardGridCache.Lock()
	defer boardGridCache.Unlock()
	if len(boardGridCache.entries) > boardGridCacheMaxEntries {
		t.Fatalf("cache must stay bounded at %d, got %d", boardGridCacheMaxEntries, len(boardGridCache.entries))
	}
	if _, ok := boardGridCache.entries["org|2026-08-0"]; ok {
		t.Fatal("the oldest entry must have been evicted")
	}
	if _, ok := boardGridCache.entries["org|2026-08-"+strconv.Itoa(boardGridCacheMaxEntries+7)]; !ok {
		t.Fatal("the newest entry must survive eviction")
	}
}
