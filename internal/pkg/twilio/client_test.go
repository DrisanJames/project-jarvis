package twilio

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNewClient_ReturnsNilWhenIncomplete(t *testing.T) {
	cases := []struct {
		name, sid, token, from string
	}{
		{"empty sid", "", "tok", "+1555"},
		{"empty token", "AC123", "", "+1555"},
		{"empty from", "AC123", "tok", ""},
		{"all empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if c := NewClient(tc.sid, tc.token, tc.from); c != nil {
				t.Fatalf("expected nil client, got %+v", c)
			}
		})
	}
}

func TestSendSMS_PostsExpectedForm(t *testing.T) {
	var (
		gotPath     string
		gotAuthHdr  string
		gotCTHdr    string
		gotTo, gotFrom, gotBody string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthHdr = r.Header.Get("Authorization")
		gotCTHdr = r.Header.Get("Content-Type")

		raw, _ := io.ReadAll(r.Body)
		vals, err := url.ParseQuery(string(raw))
		if err != nil {
			t.Fatalf("parse form: %v", err)
		}
		gotTo = vals.Get("To")
		gotFrom = vals.Get("From")
		gotBody = vals.Get("Body")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sid":"SM123","status":"queued","error_code":null,"error_message":""}`))
	}))
	defer srv.Close()

	c := NewClient("AC00000000000000000000000000000000", "token-xyz", "+15555555555")
	if c == nil {
		t.Fatalf("unexpected nil client")
	}
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()

	sid, err := c.SendSMS(context.Background(), "+18777804236", "Campaign X Did Not Send at scheduled time.")
	if err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	if sid != "SM123" {
		t.Fatalf("sid = %q, want SM123", sid)
	}
	if !strings.HasPrefix(gotPath, "/2010-04-01/Accounts/AC00000000000000000000000000000000/Messages.json") {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuthHdr, "Basic ") {
		t.Fatalf("auth header missing Basic prefix: %q", gotAuthHdr)
	}
	if gotCTHdr != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type = %q", gotCTHdr)
	}
	if gotTo != "+18777804236" {
		t.Fatalf("To = %q", gotTo)
	}
	if gotFrom != "+15555555555" {
		t.Fatalf("From = %q", gotFrom)
	}
	if !strings.Contains(gotBody, "Campaign X Did Not Send") {
		t.Fatalf("Body = %q", gotBody)
	}
}

func TestSendSMS_SurfacesAPIErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sid":"SM999","status":"failed","error_code":21610,"error_message":"The message From/To pair violates a blacklist rule."}`))
	}))
	defer srv.Close()

	c := NewClient("AC", "tok", "+1555")
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()

	_, err := c.SendSMS(context.Background(), "+1999", "hi")
	if err == nil {
		t.Fatalf("expected error from Twilio-reported failure")
	}
	if !strings.Contains(err.Error(), "21610") {
		t.Fatalf("error did not surface error_code: %v", err)
	}
}

func TestSendSMS_Non2xxIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":20003,"message":"Authenticate"}`))
	}))
	defer srv.Close()

	c := NewClient("AC", "tok", "+1555")
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()

	_, err := c.SendSMS(context.Background(), "+1999", "hi")
	if err == nil {
		t.Fatalf("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Authenticate") {
		t.Fatalf("error does not include status+body: %v", err)
	}
}

func TestSendSMS_NilClient(t *testing.T) {
	var c *Client
	if _, err := c.SendSMS(context.Background(), "+1999", "hi"); err == nil {
		t.Fatalf("expected error from nil client")
	}
}

func TestSendSMS_EmptyArgs(t *testing.T) {
	c := NewClient("AC", "tok", "+1555")
	if _, err := c.SendSMS(context.Background(), "", "hi"); err == nil {
		t.Fatalf("expected error for empty To")
	}
	if _, err := c.SendSMS(context.Background(), "+1", ""); err == nil {
		t.Fatalf("expected error for empty body")
	}
}
