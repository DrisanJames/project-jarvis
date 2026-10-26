package api

import (
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

type recNotifier struct {
	mu     sync.Mutex
	titles []string
	done   chan struct{}
}

func (n *recNotifier) Notify(title, body string) error {
	n.mu.Lock()
	n.titles = append(n.titles, title)
	n.mu.Unlock()
	close(n.done)
	return nil
}

func (n *recNotifier) Name() string { return "rec" }

func runRoute(t *testing.T, isCPC bool) (conv, click *recNotifier) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(regexp.QuoteMeta(`lower(payout_type)='cpc'`)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(isCPC))
	mock.ExpectQuery(`SELECT email FROM mailing_subscribers`).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("a@b.com"))
	mock.ExpectQuery(`SELECT name FROM mailing_offers`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Offer X"))

	conv = &recNotifier{done: make(chan struct{})}
	click = &recNotifier{done: make(chan struct{})}
	notifyConversionAsync(conv, click, db, uuid.New(), "379", 2.50, "txn1")
	select {
	case <-conv.done:
	case <-click.done:
	case <-time.After(3 * time.Second):
		t.Fatal("no notification delivered")
	}
	return conv, click
}

// A CPC payout is click revenue: it must reach the click channel and never #conversions.
func TestConversionNotify_CPCGoesToClickChannel(t *testing.T) {
	conv, click := runRoute(t, true)
	if len(conv.titles) != 0 {
		t.Fatalf("CPC payout posted to #conversions: %v", conv.titles)
	}
	if len(click.titles) != 1 {
		t.Fatalf("click channel got %d posts, want 1", len(click.titles))
	}
}

// A non-CPC payout stays in #conversions.
func TestConversionNotify_CPAStaysInConversions(t *testing.T) {
	conv, click := runRoute(t, false)
	if len(click.titles) != 0 {
		t.Fatalf("CPA payout posted to click channel: %v", click.titles)
	}
	if len(conv.titles) != 1 {
		t.Fatalf("#conversions got %d posts, want 1", len(conv.titles))
	}
}
