package api

// HTTP handler for the click/conversion attribution review tool.
//
// Mounted at POST /api/mailing/attribution/match-csv and inherits the
// auth middleware applied to /api/* by SetupRoutes (so the same login that
// gates the dashboard also gates this).

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// AttributionHandler exposes the CSV-upload endpoint. Constructed in
// server_routes_mailing.go alongside the rest of the mailing handlers so it
// shares the *sql.DB pool.
type AttributionHandler struct {
	svc *MailingService
}

// NewAttributionHandler wires the handler around an existing MailingService.
// We accept the service rather than a raw db so future additions (caching,
// org enforcement helpers) live in one place.
func NewAttributionHandler(svc *MailingService) *AttributionHandler {
	return &AttributionHandler{svc: svc}
}

// uploadLimitBytes caps the total multipart body size. Everflow exports of
// even a year of clicks rarely exceed ~50MB; 64MB is a comfortable ceiling
// without exposing us to memory-exhaustion DoS via runaway uploads.
const uploadLimitBytes = 64 << 20

// HandleMatchCSV accepts a multipart POST with parts named "clicks" and
// "conversions" (either may be absent — the matcher handles empty inputs)
// and returns a single JSON AttributionResult.
func (h *AttributionHandler) HandleMatchCSV(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil || h.svc.db == nil {
		http.Error(w, `{"error":"attribution handler not initialized"}`, http.StatusServiceUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, uploadLimitBytes)
	if err := r.ParseMultipartForm(uploadLimitBytes); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "invalid multipart upload",
			"detail": err.Error(),
		})
		return
	}

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}

	opts := AttributionOptions{
		OrgID: orgID.String(),
	}
	if pat := strings.TrimSpace(r.FormValue("offer_pattern")); pat != "" {
		opts.OfferLikePattern = pat
	}

	clicks, clicksErr := readClicksFromForm(r)
	if clicksErr != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "failed to parse clicks CSV",
			"detail": clicksErr.Error(),
		})
		return
	}

	conversions, convErr := readConversionsFromForm(r)
	if convErr != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "failed to parse conversions CSV",
			"detail": convErr.Error(),
		})
		return
	}

	if len(clicks) == 0 && len(conversions) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "neither 'clicks' nor 'conversions' file part contained any rows",
		})
		return
	}

	result, err := MatchAttribution(r.Context(), h.svc.db, clicks, conversions, opts)
	if err != nil {
		log.Printf("[Attribution] match failed org=%s clicks=%d conv=%d: %v", orgID, len(clicks), len(conversions), err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{
			"error":  "attribution match failed",
			"detail": err.Error(),
		})
		return
	}

	log.Printf("[Attribution] org=%s clicks=%d/%d conv=%d/%d offer=%q",
		orgID,
		len(result.MatchedClicks), result.TotalClicks,
		len(result.MatchedConversions), result.TotalConversions,
		result.OfferLinkPattern,
	)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Api-Version", "attribution-1.0")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("[Attribution] response encode failed: %v", err)
	}
}

func readClicksFromForm(r *http.Request) ([]ClickRow, error) {
	file, _, err := r.FormFile("clicks")
	if err == http.ErrMissingFile {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read clicks part: %w", err)
	}
	defer file.Close()
	return ParseEverflowClicksCSV(file)
}

func readConversionsFromForm(r *http.Request) ([]ConversionRow, error) {
	file, _, err := r.FormFile("conversions")
	if err == http.ErrMissingFile {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read conversions part: %w", err)
	}
	defer file.Close()
	return ParseEverflowConversionsCSV(file)
}
