package mailing

// {{ system.tracking_base }} in the preview/journey ContextBuilder mirrors
// SendWorkerPool.buildRenderContext: scheme+host, no trailing slash, absent
// when no base is configured.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestBuildSampleContext_TrackingBase(t *testing.T) {
	cb := NewContextBuilder(nil, "https://t.em.quizfiesta.com/", "k")
	sys, ok := cb.BuildSampleContext()["system"].(map[string]interface{})
	if !ok {
		t.Fatal("sample context has no system map")
	}
	if got, _ := sys["tracking_base"].(string); got != "https://t.em.quizfiesta.com" {
		t.Fatalf("sample system.tracking_base = %q, want trimmed base", got)
	}
}

func TestBuildContext_TrackingBase(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	sub := &Subscriber{ID: uuid.New(), Email: "someone@yahoo.com"}

	// Present, trimmed, and independent of campaign (nil campaign = journey path).
	cb := NewContextBuilder(db, "https://t.m.quizfiesta.com/", "k")
	rc, err := cb.BuildContext(context.Background(), sub, nil)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	sys := rc["system"].(map[string]interface{})
	if got, _ := sys["tracking_base"].(string); got != "https://t.m.quizfiesta.com" {
		t.Fatalf("system.tracking_base = %q, want %q", got, "https://t.m.quizfiesta.com")
	}

	// Empty base → key absent (negative control).
	rc2, err := NewContextBuilder(db, "", "k").BuildContext(context.Background(), sub, nil)
	if err != nil {
		t.Fatalf("BuildContext (empty base): %v", err)
	}
	if v, ok := rc2["system"].(map[string]interface{})["tracking_base"]; ok {
		t.Fatalf("system.tracking_base present with empty base: %v", v)
	}
}
