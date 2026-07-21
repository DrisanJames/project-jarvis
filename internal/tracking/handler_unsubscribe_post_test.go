package tracking

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Pins the 2026-07-21 RFC 8058 fix. Google's one-click compliance check POSTs
// to the List-Unsubscribe https URL with body "List-Unsubscribe=One-Click";
// this service — the one the public trk./t.em tracking hosts route to —
// registered the unsubscribe route as GET-only, so chi answered 405 Method
// Not Allowed (live-verified 2026-07-21 against trk.em.discountblog.com,
// trk.em.quizfiesta.com, trk.em.bestcreditcare.com and projectjarvis.io) and
// every sending domain failed Google Postmaster compliance. The POST must be
// accepted and must record the unsubscribe with NO confirmation step.

func testHandler() *Handler {
	// Real (non-nil) SQS client pointed at an unroutable local endpoint: the
	// async Publish goroutine fails fast and logs; the handler's HTTP
	// behavior — all this test asserts — is independent of publish success.
	client := sqs.New(sqs.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://127.0.0.1:1")})
	return NewHandler(NewPublisher(client, "http://127.0.0.1:1/queue"))
}

func token(parts ...string) string {
	return base64.URLEncoding.EncodeToString([]byte(strings.Join(parts, "|")))
}

func TestHandleUnsubscribe_OneClickPOST(t *testing.T) {
	srv := httptest.NewServer(testHandler().Routes())
	defer srv.Close()

	// 3-part global token, signed URL shape (the shape emitted in the
	// List-Unsubscribe https leg always carries a sig segment).
	data := token(
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
	)
	resp, err := http.Post(
		srv.URL+"/track/unsubscribe/"+data+"/somesig",
		"application/x-www-form-urlencoded",
		strings.NewReader("List-Unsubscribe=One-Click"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("one-click POST = %d, want 200 (405 here is the exact Google Postmaster failure)", resp.StatusCode)
	}
}

func TestHandleUnsubscribe_OneClickPOST_BrandToken(t *testing.T) {
	srv := httptest.NewServer(testHandler().Routes())
	defer srv.Close()

	// 4-part brand-scoped token — the shape worker.GenerateBrandUnsubscribeURL
	// puts in the header's https leg (parts[3] is a brand root, NOT an email
	// id UUID).
	data := token(
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
		"quizfiesta.com",
	)
	resp, err := http.Post(
		srv.URL+"/track/unsubscribe/"+data+"/somesig",
		"application/x-www-form-urlencoded",
		strings.NewReader("List-Unsubscribe=One-Click"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("brand-token one-click POST = %d, want 200", resp.StatusCode)
	}
}

func TestHandleUnsubscribe_POST_NoSigVariant(t *testing.T) {
	srv := httptest.NewServer(testHandler().Routes())
	defer srv.Close()

	data := token("o", "c", "s")
	resp, err := http.Post(srv.URL+"/track/unsubscribe/"+data, "application/x-www-form-urlencoded", strings.NewReader("List-Unsubscribe=One-Click"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sig-less one-click POST = %d, want 200 (parity with the API server's registration)", resp.StatusCode)
	}
}

func TestHandleUnsubscribe_GETStillServesConfirmationPage(t *testing.T) {
	srv := httptest.NewServer(testHandler().Routes())
	defer srv.Close()

	data := token("o", "c", "s")
	resp, err := http.Get(srv.URL + "/track/unsubscribe/" + data + "/somesig")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("GET should keep serving the human HTML page, got Content-Type %q", ct)
	}
}

func TestHandleUnsubscribe_POST_BadToken(t *testing.T) {
	srv := httptest.NewServer(testHandler().Routes())
	defer srv.Close()

	// "abc" is not valid padded base64url (length 3), so decode fails.
	resp, err := http.Post(srv.URL+"/track/unsubscribe/abc/sig", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-token POST = %d, want 400", resp.StatusCode)
	}
}
