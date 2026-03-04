package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendViaPMTAAPI_WithVMTA(t *testing.T) {
	var capturedPayload map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedPayload)
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	svc := &MailingService{}
	headers := map[string]string{
		"X-Virtual-MTA": "yahoo-pool",
		"X-Job":         "test-campaign",
	}

	_, err := svc.sendViaPMTAAPI(context.Background(), server.URL, "user@yahoo.com", "from@example.com", "Sender", "", "Test Subject", "<p>Hello</p>", "Hello", headers)
	if err != nil {
		t.Fatalf("sendViaPMTAAPI failed: %v", err)
	}

	vmta, ok := capturedPayload["vmta"]
	if !ok {
		t.Fatal("vmta field missing from payload")
	}
	if vmta != "yahoo-pool" {
		t.Errorf("vmta: want yahoo-pool, got %v", vmta)
	}
}

func TestSendViaPMTAAPI_WithoutVMTA(t *testing.T) {
	var capturedPayload map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedPayload)
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	svc := &MailingService{}
	headers := map[string]string{
		"X-Job": "test-campaign",
	}

	_, err := svc.sendViaPMTAAPI(context.Background(), server.URL, "user@yahoo.com", "from@example.com", "Sender", "", "Test Subject", "<p>Hello</p>", "Hello", headers)
	if err != nil {
		t.Fatalf("sendViaPMTAAPI failed: %v", err)
	}

	if _, ok := capturedPayload["vmta"]; ok {
		t.Error("vmta field should not be present when no X-Virtual-MTA header is set")
	}
}
