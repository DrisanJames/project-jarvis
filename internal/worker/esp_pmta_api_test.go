package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

	sender := NewPMTAAPISender(server.URL, nil)

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

func TestPMTAAPISender_WithoutVMTA(t *testing.T) {
	var capturedPayload map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedPayload)
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	sender := NewPMTAAPISender(server.URL, nil)

	msg := &EmailMessage{
		Email:       "user@gmail.com",
		FromName:    "Test",
		FromEmail:   "test@example.com",
		Subject:     "Hello",
		HTMLContent: "<p>Test</p>",
		Headers:     map[string]string{},
	}

	_, err := sender.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if _, ok := capturedPayload["vmta"]; ok {
		t.Error("vmta field should not be present when no X-Virtual-MTA header is set")
	}
}
