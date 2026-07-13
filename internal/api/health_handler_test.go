package api

// Gate C ("Delivery Build Check") backend truth source — see
// deliveryBuildFlags in health_handler.go. The Send-Day Planner reads
// build_contains[GATE_C_REQUIRED_COMMIT] from /health; a build lacking
// a92af78 also lacks this map, so the chip goes red on absence. These
// tests pin the contract: the flag exists, is true, and is emitted by
// the registered /health handler (server_setters.go:304).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeliveryBuildFlags_ContainsGateCRequiredCommit(t *testing.T) {
	// The required commit for Gate C is a92af78 (IsPMTATransient dead-letter
	// classifier fix, CLAUDE.md §6 Gate C). Must match the frontend constant
	// GATE_C_REQUIRED_COMMIT in send-day-planner/constants.ts.
	assert.Equal(t, "a92af78", gateCRequiredCommit)
	assert.True(t, deliveryBuildFlags[gateCRequiredCommit],
		"deliveryBuildFlags must report the dead-letter classifier commit as present")
}

func TestHandleHealth_EmitsBuildContainsForGateC(t *testing.T) {
	hc := NewHealthChecker(nil, nil, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	hc.HandleHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	contains, ok := resp["build_contains"].(map[string]interface{})
	require.True(t, ok, "/health must emit build_contains for Send-Day Gate C, got: %s", rec.Body.String())
	flag, ok := contains[gateCRequiredCommit].(bool)
	require.True(t, ok, "build_contains must carry %q", gateCRequiredCommit)
	assert.True(t, flag)
}
