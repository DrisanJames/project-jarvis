package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPMTAAPISender_WithVMTA(t *testing.T) {
	var capturedPayload map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedPayload)
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	sender := NewPMTAAPISender(server.URL, nil, "")

	msg := &EmailMessage{
		Email:       "user@gmail.com",
		FromName:    "Test",
		FromEmail:   "test@example.com",
		Subject:     "Hello",
		HTMLContent: "<p>Test</p>",
		Headers: map[string]string{
			"X-Virtual-MTA": "gmail-pool",
		},
	}

	_, err := sender.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	vmta, ok := capturedPayload["vmta"]
	if !ok {
		t.Fatal("vmta field missing from PMTA API payload")
	}
	if vmta != "gmail-pool" {
		t.Errorf("vmta: want gmail-pool, got %v", vmta)
	}
}

func TestPMTAAPISender_WithoutVMTA_RejectsDefaultPool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	sender := NewPMTAAPISender(server.URL, nil, "")

	msg := &EmailMessage{
		Email:       "user@gmail.com",
		FromName:    "Test",
		FromEmail:   "test@example.com",
		Subject:     "Hello",
		HTMLContent: "<p>Test</p>",
		Headers:     map[string]string{},
	}

	_, err := sender.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error when no VMTA routing is available, but Send succeeded")
	}
	if !strings.Contains(err.Error(), "refusing to send") {
		t.Errorf("expected 'refusing to send' in error, got: %v", err)
	}
}

func TestPMTAAPISender_EnvelopeSender(t *testing.T) {
	var capturedPayload map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedPayload)
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	sender := NewPMTAAPISender(server.URL, nil, "")
	msg := &EmailMessage{
		Email:       "recipient@gmail.com",
		FromName:    "Sender",
		FromEmail:   "sender@example.com",
		Subject:     "Envelope Test",
		HTMLContent: "<p>Test</p>",
		Headers:     map[string]string{"X-Virtual-MTA": "pool-1"},
	}

	_, err := sender.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	envSender, ok := capturedPayload["envelope_sender"]
	if !ok {
		t.Fatal("envelope_sender field missing from PMTA API payload")
	}
	if envSender != "sender@example.com" {
		t.Errorf("envelope_sender: want sender@example.com, got %v", envSender)
	}
}

func TestPMTAAPISender_ContentContainsRFC822Headers(t *testing.T) {
	var capturedPayload map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedPayload)
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	sender := NewPMTAAPISender(server.URL, nil, "")
	msg := &EmailMessage{
		Email:       "test@gmail.com",
		FromName:    "RFC Test",
		FromEmail:   "rfc@example.com",
		Subject:     "RFC822 Check",
		HTMLContent: "<html><body>Hello</body></html>",
		Headers:     map[string]string{"X-Virtual-MTA": "pool-1"},
	}

	_, err := sender.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	content, ok := capturedPayload["content"].(string)
	if !ok {
		t.Fatal("content field missing or not a string")
	}

	requiredHeaders := []string{
		"From:",
		"To:",
		"Subject:",
		"MIME-Version:",
		"Content-Type:",
	}
	for _, h := range requiredHeaders {
		if !strings.Contains(content, h) {
			t.Errorf("RFC822 content missing header %q", h)
		}
	}
}

func TestPMTAAPISender_RecipientsField(t *testing.T) {
	var capturedPayload map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedPayload)
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	sender := NewPMTAAPISender(server.URL, nil, "")
	msg := &EmailMessage{
		Email:       "target@yahoo.com",
		FromName:    "Test",
		FromEmail:   "test@example.com",
		Subject:     "Recipients Test",
		HTMLContent: "<p>Test</p>",
		Headers:     map[string]string{"X-Virtual-MTA": "pool-1"},
	}

	_, err := sender.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	recipients, ok := capturedPayload["recipients"]
	if !ok {
		t.Fatal("recipients field missing from payload")
	}
	recList, ok := recipients.([]interface{})
	if !ok {
		t.Fatalf("recipients is not an array: %T", recipients)
	}
	if len(recList) != 1 {
		t.Errorf("expected 1 recipient, got %d", len(recList))
	}
}

// TestPMTAAPISender_ZeroSuccessCountIsError guards the silent false-success
// class from the 2026-07-01 incident: the inject API answers HTTP 200 with
// {"success_count":0,...} when every recipient fails (e.g. unparseable From
// header rejected by DKIM policy). That must surface as a send error, never
// a success.
func TestPMTAAPISender_ZeroSuccessCountIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"success_count":0,"fail_count":1,"failed_recipients":["u@gmail.com"],"errors":["u@gmail.com: 552 5.6.0 DKIM signing requires a From header, but it is missing from this message"]}`))
	}))
	defer server.Close()

	sender := NewKumoAPISender(server.URL, nil, "")
	msg := &EmailMessage{
		Email: "u@gmail.com", FromName: "Grace @ Best Credit Care",
		FromEmail: "hello@em.bestcreditcare.com", Subject: "Hi", HTMLContent: "<p>x</p>",
		Headers: map[string]string{"X-Virtual-MTA": "mta-bcc-gm2"},
	}
	res, err := sender.Send(context.Background(), msg)
	if err == nil {
		t.Fatalf("want error on success_count=0, got success: %+v", res)
	}
	if !strings.Contains(err.Error(), "inject accepted 0 recipients") {
		t.Errorf("error should carry inject failure detail, got: %v", err)
	}
}

// A body with success_count>=1 (or no success_count field at all — the PMTA
// bridge's legacy shapes) must keep succeeding.
func TestPMTAAPISender_SuccessCountOKAndLegacyBodies(t *testing.T) {
	for _, body := range []string{`{"success_count":1,"fail_count":0,"errors":[]}`, `{"status":"ok"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(body))
		}))
		sender := NewKumoAPISender(server.URL, nil, "")
		msg := &EmailMessage{
			Email: "u@gmail.com", FromName: "T", FromEmail: "t@x.com", Subject: "s", HTMLContent: "<p>x</p>",
			Headers: map[string]string{"X-Virtual-MTA": "mta-x-gm1"},
		}
		if _, err := sender.Send(context.Background(), msg); err != nil {
			t.Errorf("body %s: want success, got %v", body, err)
		}
		server.Close()
	}
}
