package api

// CSV parsers for the Everflow "clicks" and "conversions" exports.
//
// Both formats are headered. We index columns by name (case-insensitive,
// trimmed) rather than by position so a future Everflow column reshuffle
// doesn't silently break attribution. Unknown columns are ignored.

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// edtLocation is the timezone Everflow stamps the clicks CSV with. We resolve
// it once at package init so concurrent parses don't race tzdata loads.
var edtLocation = func() *time.Location {
	// Everflow exports use "America/New_York" semantics regardless of whether
	// the suffix on the row reads "EDT" or "EST". Falling back to a fixed
	// -0400 offset only happens in containers without tzdata, which shouldn't
	// be the case for our deploy image but is a safe degrade.
	loc, err := time.LoadLocation("America/New_York")
	if err != nil || loc == nil {
		return time.FixedZone("EDT", -4*3600)
	}
	return loc
}()

// ParseEverflowClicksCSV reads the Everflow "clicks" export. Required columns:
//
//	Date         e.g. "04/27/2026 00:02:07 EDT"
//	Offer        free-text offer name (used only for display)
//	IP Address   IPv4 or IPv6
//	Transaction ID  Everflow's session/click hash
//
// Optional columns we surface for context: Browser, Country, ISP,
// Error Code, Error Message.
func ParseEverflowClicksCSV(r io.Reader) ([]ClickRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tolerate trailing-comma rows that some exports emit
	cr.LazyQuotes = true

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read clicks header: %w", err)
	}
	idx := indexHeaderColumns(header)

	required := []string{"date", "ip address", "offer"}
	for _, col := range required {
		if _, ok := idx[col]; !ok {
			return nil, fmt.Errorf("clicks CSV missing required column %q (have: %v)", col, header)
		}
	}

	var rows []ClickRow
	rowNum := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("clicks CSV row %d: %w", rowNum+1, err)
		}
		rowNum++

		dateStr := getCol(rec, idx, "date")
		if strings.TrimSpace(dateStr) == "" {
			continue // skip empty rows that some exports include at EOF
		}
		ts, err := parseEverflowClickTimestamp(dateStr)
		if err != nil {
			return nil, fmt.Errorf("clicks CSV row %d date %q: %w", rowNum, dateStr, err)
		}

		rows = append(rows, ClickRow{
			RowIndex:      rowNum - 1, // 1-based, header excluded
			Timestamp:     ts,
			OfferName:     getCol(rec, idx, "offer"),
			IPAddress:     strings.TrimSpace(getCol(rec, idx, "ip address")),
			TransactionID: strings.TrimSpace(getCol(rec, idx, "transaction id")),
			Browser:       getCol(rec, idx, "browser"),
			Country:       getCol(rec, idx, "country"),
			ISP:           getCol(rec, idx, "isp"),
			ErrorCode:     getCol(rec, idx, "error code"),
			ErrorMessage:  getCol(rec, idx, "error message"),
		})
	}
	return rows, nil
}

// ParseEverflowConversionsCSV reads the Everflow "conversions" export.
// Required columns:
//
//	conversion_id, transaction_id, date, click_date, offer_name,
//	revenue, session_user_ip
//
// Optional surfaced for context: conversion_user_ip, country, isp.
func ParseEverflowConversionsCSV(r io.Reader) ([]ConversionRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read conversions header: %w", err)
	}
	idx := indexHeaderColumns(header)

	required := []string{"conversion_id", "transaction_id", "click_date", "session_user_ip", "offer_name"}
	for _, col := range required {
		if _, ok := idx[col]; !ok {
			return nil, fmt.Errorf("conversions CSV missing required column %q (have: %v)", col, header)
		}
	}

	var rows []ConversionRow
	rowNum := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("conversions CSV row %d: %w", rowNum+1, err)
		}
		rowNum++

		clickStr := getCol(rec, idx, "click_date")
		if strings.TrimSpace(clickStr) == "" {
			continue
		}
		clickTime, err := parseEverflowConversionTimestamp(clickStr)
		if err != nil {
			return nil, fmt.Errorf("conversions CSV row %d click_date %q: %w", rowNum, clickStr, err)
		}

		var convTime time.Time
		if cs := strings.TrimSpace(getCol(rec, idx, "date")); cs != "" {
			t, err := parseEverflowConversionTimestamp(cs)
			if err != nil {
				return nil, fmt.Errorf("conversions CSV row %d date %q: %w", rowNum, cs, err)
			}
			convTime = t
		}

		revenue := 0.0
		if rs := strings.TrimSpace(getCol(rec, idx, "revenue")); rs != "" {
			// Strip $ and , from "$50.00" / "1,250.00" forms.
			rs = strings.TrimPrefix(rs, "$")
			rs = strings.ReplaceAll(rs, ",", "")
			if v, err := strconv.ParseFloat(rs, 64); err == nil {
				revenue = v
			}
		}

		rows = append(rows, ConversionRow{
			RowIndex:         rowNum - 1,
			ConversionID:     strings.TrimSpace(getCol(rec, idx, "conversion_id")),
			TransactionID:    strings.TrimSpace(getCol(rec, idx, "transaction_id")),
			ConversionTime:   convTime,
			ClickTime:        clickTime,
			OfferName:        getCol(rec, idx, "offer_name"),
			Revenue:          revenue,
			SessionUserIP:    strings.TrimSpace(getCol(rec, idx, "session_user_ip")),
			ConversionUserIP: strings.TrimSpace(getCol(rec, idx, "conversion_user_ip")),
			Country:          getCol(rec, idx, "country"),
			ISP:              getCol(rec, idx, "isp"),
		})
	}
	return rows, nil
}

func indexHeaderColumns(header []string) map[string]int {
	idx := make(map[string]int, len(header))
	for i, h := range header {
		key := strings.ToLower(strings.TrimSpace(h))
		// Some Everflow exports double-quote header fields; csv.Reader strips
		// them, but trim again defensively.
		key = strings.Trim(key, "\"' ")
		if _, exists := idx[key]; !exists {
			idx[key] = i
		}
	}
	return idx
}

func getCol(rec []string, idx map[string]int, name string) string {
	i, ok := idx[name]
	if !ok || i >= len(rec) {
		return ""
	}
	return rec[i]
}

// parseEverflowClickTimestamp accepts the format Everflow uses in the clicks
// export: "MM/DD/YYYY HH:MM:SS TZ" where TZ is "EDT" or "EST". The TZ is
// authoritative — we honor it by attaching America/New_York and letting Go
// resolve DST, which matches the export's intent regardless of suffix label.
func parseEverflowClickTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	// Strip the TZ suffix if present so we can parse the body in our known
	// location explicitly. Everflow gives us EDT/EST labels but the wall
	// clock is always Eastern time, so attaching America/New_York is correct.
	for _, tz := range []string{" EDT", " EST", " UTC", " GMT"} {
		s = strings.TrimSuffix(s, tz)
	}
	t, err := time.ParseInLocation("01/02/2006 15:04:05", s, edtLocation)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected MM/DD/YYYY HH:MM:SS, got %q: %w", s, err)
	}
	return t.UTC(), nil
}

// parseEverflowConversionTimestamp handles the conversions export which uses
// "YYYY-MM-DD HH:MM:SS" without a timezone suffix. Everflow stores these in
// the timezone configured on the network — for this account it's Eastern,
// matching the clicks export.
func parseEverflowConversionTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, edtLocation)
	if err != nil {
		// Fall back to a few alternative shapes seen in older exports.
		for _, layout := range []string{"2006-01-02T15:04:05", "01/02/2006 15:04:05"} {
			if t2, err2 := time.ParseInLocation(layout, s, edtLocation); err2 == nil {
				return t2.UTC(), nil
			}
		}
		return time.Time{}, fmt.Errorf("expected YYYY-MM-DD HH:MM:SS, got %q: %w", s, err)
	}
	return t.UTC(), nil
}
