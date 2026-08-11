package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The 2026-08-11 internal_auto_insurance loss: two wave groups of the same
// (vertical, brand, creative) deployed one second apart produced a byte-
// identical campaign name, HandleDeployCampaign's by-(org,name) idempotency
// guard returned the FIRST campaign to the second group, and 349 records were
// marked mailed against a 2-recipient campaign. These pin both halves of the
// fix.

// Negative control: revert the segmentID/seconds nonce in buildCampaignInput
// and this fails — both names collapse to the same string.
func TestWaveCampaignName_UniquePerSegment(t *testing.T) {
	po := &PartnerDripOrchestrator{cfg: PartnerDripOrchestratorConfig{WindowHours: 16}}
	v := verticalState{vertical: "internal_auto_insurance"}
	creative := creativeRec{
		fromName: "Discount Auto Insurance",
		subject:  `{{ first_name | default: "Your" }}, your quotes are ready`,
		htmlBody: "<html>identical creative for both groups</html>",
	}
	counts := map[string]int{"aol": 349}

	// Two groups, same vertical/brand/creative, deployed in the same second —
	// the exact shape that collided in production.
	a, err := po.buildCampaignInput(v, "db", creative, "6134bdc3-ba7f-4144-8a54-fed14bd336de", counts)
	require.NoError(t, err)
	b, err := po.buildCampaignInput(v, "db", creative, "cb10266a-6f83-4702-a181-79c607a1c4e8", counts)
	require.NoError(t, err)

	assert.NotEqual(t, a.Name, b.Name,
		"two wave groups in the same second MUST NOT share a campaign name — the deploy guard would converge them and burn a group's records")
	assert.Contains(t, a.Name, "6134bdc3", "name carries its own segment nonce")
	assert.Contains(t, b.Name, "cb10266a", "name carries its own segment nonce")
}

// A wave whose deploy converges on an existing campaign must surface an error,
// NOT a campaign id — deployWaveGroups keys off the error to releaseClaim
// instead of markMailed. Negative control: drop the already_existed check in
// WrapPMTACampaignDeploy and this fails.
func TestWrapPMTACampaignDeploy_AlreadyExistedIsAnError(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"campaign_id":     "caa1f42d-86da-4de7-842d-e582e4234d99",
			"status":          "scheduled",
			"already_existed": true,
		})
	}

	id, err := WrapPMTACampaignDeploy(handler)(context.Background(), engine.PMTACampaignInput{Name: "collides"})

	require.Error(t, err, "a name-converged deploy must not look like a success")
	assert.True(t, errors.Is(err, ErrDeployNameReused), "must be the typed sentinel so callers can branch")
	assert.Empty(t, id, "no campaign id may be returned — returning one is what caused markMailed to burn records")
	assert.True(t, strings.Contains(err.Error(), "caa1f42d"), "error names the campaign it converged on, for triage")
}

// The normal path must be untouched: a freshly minted campaign returns its id
// and no error.
func TestWrapPMTACampaignDeploy_FreshDeployUnaffected(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"campaign_id":     "11111111-2222-3333-4444-555555555555",
			"already_existed": false,
		})
	}
	id, err := WrapPMTACampaignDeploy(handler)(context.Background(), engine.PMTACampaignInput{Name: "fresh"})
	require.NoError(t, err)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", id)
}
