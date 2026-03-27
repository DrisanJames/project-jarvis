package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/datanorm"
)

// SiteEventsHandler accepts engagement events from owned websites (H9, H10).
type SiteEventsHandler struct {
	db          *sql.DB
	eventWriter *datanorm.EventWriter
	recentSeen  map[string]time.Time
	mu          sync.Mutex

	activeNow   int64 // atomic: visitors seen in the last 60s
	activeBuf   map[string]time.Time
	activeMu    sync.Mutex
	sseClients  map[chan []byte]struct{}
	sseMu       sync.RWMutex
}

func NewSiteEventsHandler(db *sql.DB, ew *datanorm.EventWriter) *SiteEventsHandler {
	h := &SiteEventsHandler{
		db:          db,
		eventWriter: ew,
		recentSeen:  make(map[string]time.Time),
		activeBuf:   make(map[string]time.Time),
		sseClients:  make(map[chan []byte]struct{}),
	}
	go h.cleanupLoop()
	return h
}

func (h *SiteEventsHandler) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Second)
	for range ticker.C {
		now := time.Now()

		h.mu.Lock()
		for k, v := range h.recentSeen {
			if now.Sub(v) > 30*time.Second {
				delete(h.recentSeen, k)
			}
		}
		h.mu.Unlock()

		h.activeMu.Lock()
		var count int64
		for k, v := range h.activeBuf {
			if now.Sub(v) > 60*time.Second {
				delete(h.activeBuf, k)
			} else {
				count++
			}
		}
		h.activeMu.Unlock()
		atomic.StoreInt64(&h.activeNow, count)
	}
}

type siteEventPayload struct {
	EID       string                 `json:"eid"`
	CID       string                 `json:"cid"`
	SID       string                 `json:"sid"`
	EventType string                 `json:"event_type"`
	PageURL   string                 `json:"page_url"`
	PageTitle string                 `json:"page_title"`
	Referrer  string                 `json:"referrer"`
	Domain    string                 `json:"domain"`
	Metadata  map[string]interface{} `json:"metadata"`
}

// HandleSiteEvent handles POST /api/v1/site-events.
// Accepts both application/json and text/plain (for sendBeacon).
func (h *SiteEventsHandler) HandleSiteEvent(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	referer := r.Header.Get("Referer")
	if referer != "" && !isAllowedReferer(referer) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ua := r.UserAgent()
	if isBot(ua) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var payload siteEventPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	if payload.EventType == "" {
		payload.EventType = "page_view"
	}

	pagePath := ""
	if payload.PageURL != "" {
		pagePath = payload.PageURL
	} else if payload.Metadata != nil {
		if pp, ok := payload.Metadata["page_path"].(string); ok {
			pagePath = pp
		}
	}

	h.mu.Lock()
	dedupKey := fmt.Sprintf("%s:%s:%s", payload.EID, payload.EventType, pagePath)
	if lastSeen, ok := h.recentSeen[dedupKey]; ok && time.Since(lastSeen) < 30*time.Second {
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.recentSeen[dedupKey] = time.Now()
	h.mu.Unlock()

	visitorID := payload.EID
	if payload.SID != "" {
		visitorID = payload.SID
	}
	if visitorID == "" {
		visitorID = r.RemoteAddr
	}

	h.activeMu.Lock()
	h.activeBuf[visitorID] = time.Now()
	h.activeMu.Unlock()

	meta := payload.Metadata
	if meta == nil {
		meta = make(map[string]interface{})
	}
	meta["page_url"] = payload.PageURL
	meta["page_title"] = payload.PageTitle
	meta["referrer"] = payload.Referrer
	meta["domain"] = payload.Domain
	meta["user_agent"] = ua
	meta["ip"] = extractIPFromRemoteAddr(r.RemoteAddr)

	// Resolve subscriber identity from SID
	var subscriberEmail string
	var subscriberUUID *string
	if payload.SID != "" {
		var email string
		err := h.db.QueryRowContext(r.Context(),
			`SELECT email FROM mailing_subscribers WHERE id = $1`, payload.SID).Scan(&email)
		if err == nil && email != "" {
			subscriberEmail = email
			subscriberUUID = &payload.SID
			meta["subscriber_email"] = email
			meta["subscriber_id"] = payload.SID
		}
	}
	if payload.CID != "" {
		meta["campaign_id"] = payload.CID
	}

	emailHash := siteEventHash(visitorID)

	// Write to subscriber_events with identity columns
	ip := extractIPFromRemoteAddr(r.RemoteAddr)
	_, writeErr := h.db.ExecContext(r.Context(),
		`INSERT INTO subscriber_events (email_hash, event_type, campaign_id, source, metadata, event_at, subscriber_id, subscriber_email, ip_address, user_agent)
		 VALUES ($1, $2, $3, 'site', $4, NOW(), $5, $6, $7, $8)`,
		emailHash, payload.EventType,
		nullUUID(payload.CID),
		metaJSON(meta),
		nullUUID(safeDeref(subscriberUUID)),
		nullString(subscriberEmail),
		ip, ua)
	if writeErr != nil {
		log.Printf("[SitePixel] write error: %v", writeErr)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.broadcastSSE(payload.EventType, payload.Domain, pagePath, payload.PageTitle)
	w.WriteHeader(http.StatusNoContent)
}

func setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" && isAllowedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
	}
}

func isAllowedOrigin(origin string) bool {
	lower := strings.ToLower(origin)
	for _, d := range allowedRefererDomains {
		if strings.Contains(lower, d) {
			return true
		}
	}
	return false
}

// HandleSiteEventBeacon handles GET /api/v1/site-events/beacon (img pixel fallback).
func (h *SiteEventsHandler) HandleSiteEventBeacon(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w, r)

	eid := r.URL.Query().Get("eid")
	sid := r.URL.Query().Get("sid")
	cid := r.URL.Query().Get("cid")
	eventType := r.URL.Query().Get("event_type")
	if eventType == "" {
		eventType = "page_view"
	}
	domain := r.URL.Query().Get("d")
	pagePath := r.URL.Query().Get("p")
	pageTitle := r.URL.Query().Get("t")
	ip := extractIPFromRemoteAddr(r.RemoteAddr)
	ua := r.UserAgent()

	if eid != "" {
		emailHash := siteEventHash(eid)
		metadata := map[string]interface{}{
			"page_url":   pagePath,
			"page_title": pageTitle,
			"domain":     domain,
			"ip":         ip,
			"user_agent": ua,
		}

		var subscriberEmail string
		var subscriberUUID *string
		if sid != "" {
			var email string
			if h.db.QueryRowContext(r.Context(), `SELECT email FROM mailing_subscribers WHERE id = $1`, sid).Scan(&email) == nil && email != "" {
				subscriberEmail = email
				subscriberUUID = &sid
				metadata["subscriber_email"] = email
				metadata["subscriber_id"] = sid
			}
		}
		if cid != "" {
			metadata["campaign_id"] = cid
		}

		h.db.ExecContext(r.Context(),
			`INSERT INTO subscriber_events (email_hash, event_type, campaign_id, source, metadata, event_at, subscriber_id, subscriber_email, ip_address, user_agent)
			 VALUES ($1, $2, $3, 'site_beacon', $4, NOW(), $5, $6, $7, $8)`,
			emailHash, eventType,
			nullUUID(cid),
			metaJSON(metadata),
			nullUUID(safeDeref(subscriberUUID)),
			nullString(subscriberEmail),
			ip, ua)

		h.activeMu.Lock()
		h.activeBuf[eid] = time.Now()
		h.activeMu.Unlock()

		h.broadcastSSE(eventType, domain, pagePath, pageTitle)
	}

	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Write(transparentGIF)
}

// HandleGetPixelSnippet returns the JavaScript tracking snippet for a given domain.
// GET /api/mailing/site-pixel/snippet?domain=discountblog.com
func (h *SiteEventsHandler) HandleGetPixelSnippet(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "domain is required"})
		return
	}

	apiBase := r.URL.Query().Get("api_base")
	if apiBase == "" {
		apiBase = "https://projectjarvis.io"
	}

	snippet := generateTrackingSnippet(domain, apiBase)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"domain":  domain,
		"snippet": snippet,
		"instructions": map[string]string{
			"step_1": "Copy the JavaScript snippet below",
			"step_2": "Paste it before the closing </body> tag on every page of " + domain,
			"step_3": "Traffic will appear in real-time on the Jarvis dashboard",
		},
	})
}

// HandleGetSiteTraffic returns real-time and historical site traffic metrics.
// GET /api/mailing/site-pixel/traffic?domain=discountblog.com&range=24h
func (h *SiteEventsHandler) HandleGetSiteTraffic(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	timeRange := r.URL.Query().Get("range")
	if timeRange == "" {
		timeRange = "24h"
	}

	var interval string
	switch timeRange {
	case "1h":
		interval = "1 hour"
	case "24h":
		interval = "24 hours"
	case "7d":
		interval = "7 days"
	case "30d":
		interval = "30 days"
	default:
		interval = "24 hours"
	}

	activeVisitors := atomic.LoadInt64(&h.activeNow)

	var totalPageviews, uniqueVisitors int
	domainFilter := ""
	args := []interface{}{interval}
	if domain != "" {
		domainFilter = " AND metadata->>'domain' = $2"
		args = append(args, domain)
	}

	h.db.QueryRowContext(r.Context(), `
		SELECT COUNT(*), COUNT(DISTINCT email_hash)
		FROM subscriber_events
		WHERE source IN ('site','site_beacon')
		  AND event_type = 'page_view'
		  AND event_at > NOW() - $1::interval`+domainFilter,
		args...,
	).Scan(&totalPageviews, &uniqueVisitors)

	type pageHit struct {
		Path  string `json:"path"`
		Title string `json:"title"`
		Count int    `json:"count"`
	}
	var topPages []pageHit
	pageArgs := []interface{}{interval}
	pageFilter := ""
	if domain != "" {
		pageFilter = " AND metadata->>'domain' = $2"
		pageArgs = append(pageArgs, domain)
	}
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT COALESCE(metadata->>'page_url','unknown') AS path,
		       COALESCE(metadata->>'page_title','') AS title,
		       COUNT(*) AS cnt
		FROM subscriber_events
		WHERE source IN ('site','site_beacon')
		  AND event_type = 'page_view'
		  AND event_at > NOW() - $1::interval`+pageFilter+`
		GROUP BY path, title ORDER BY cnt DESC LIMIT 20`,
		pageArgs...,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var p pageHit
			rows.Scan(&p.Path, &p.Title, &p.Count)
			topPages = append(topPages, p)
		}
	}

	type hourBucket struct {
		Hour  string `json:"hour"`
		Count int    `json:"count"`
	}
	var hourly []hourBucket
	hArgs := []interface{}{interval}
	hFilter := ""
	if domain != "" {
		hFilter = " AND metadata->>'domain' = $2"
		hArgs = append(hArgs, domain)
	}
	hRows, hErr := h.db.QueryContext(r.Context(), `
		SELECT date_trunc('hour', event_at)::text AS hr, COUNT(*) AS cnt
		FROM subscriber_events
		WHERE source IN ('site','site_beacon')
		  AND event_type = 'page_view'
		  AND event_at > NOW() - $1::interval`+hFilter+`
		GROUP BY hr ORDER BY hr`,
		hArgs...,
	)
	if hErr == nil {
		defer hRows.Close()
		for hRows.Next() {
			var b hourBucket
			hRows.Scan(&b.Hour, &b.Count)
			hourly = append(hourly, b)
		}
	}

	type refHit struct {
		Referrer string `json:"referrer"`
		Count    int    `json:"count"`
	}
	var topReferrers []refHit
	rArgs := []interface{}{interval}
	rFilter := ""
	if domain != "" {
		rFilter = " AND metadata->>'domain' = $2"
		rArgs = append(rArgs, domain)
	}
	rRows, rErr := h.db.QueryContext(r.Context(), `
		SELECT COALESCE(metadata->>'referrer','direct') AS ref, COUNT(*) AS cnt
		FROM subscriber_events
		WHERE source IN ('site','site_beacon')
		  AND event_type = 'page_view'
		  AND event_at > NOW() - $1::interval`+rFilter+`
		  AND metadata->>'referrer' IS NOT NULL
		  AND metadata->>'referrer' != ''
		GROUP BY ref ORDER BY cnt DESC LIMIT 10`,
		rArgs...,
	)
	if rErr == nil {
		defer rRows.Close()
		for rRows.Next() {
			var rh refHit
			rRows.Scan(&rh.Referrer, &rh.Count)
			topReferrers = append(topReferrers, rh)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"active_visitors":  activeVisitors,
		"total_pageviews":  totalPageviews,
		"unique_visitors":  uniqueVisitors,
		"time_range":       timeRange,
		"domain":           domain,
		"top_pages":        topPages,
		"hourly_breakdown": hourly,
		"top_referrers":    topReferrers,
	})
}

// HandleSiteTrafficStream provides SSE real-time stream of site events.
// GET /api/mailing/site-pixel/traffic/stream
func (h *SiteEventsHandler) HandleSiteTrafficStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 64)
	h.sseMu.Lock()
	h.sseClients[ch] = struct{}{}
	h.sseMu.Unlock()

	defer func() {
		h.sseMu.Lock()
		delete(h.sseClients, ch)
		h.sseMu.Unlock()
		close(ch)
	}()

	active := atomic.LoadInt64(&h.activeNow)
	fmt.Fprintf(w, "data: {\"type\":\"init\",\"active_visitors\":%d}\n\n", active)
	flusher.Flush()

	ctx := r.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			active := atomic.LoadInt64(&h.activeNow)
			fmt.Fprintf(w, "data: {\"type\":\"heartbeat\",\"active_visitors\":%d}\n\n", active)
			flusher.Flush()
		}
	}
}

func (h *SiteEventsHandler) broadcastSSE(eventType, domain, pagePath, pageTitle string) {
	h.sseMu.RLock()
	defer h.sseMu.RUnlock()
	if len(h.sseClients) == 0 {
		return
	}

	msg, _ := json.Marshal(map[string]interface{}{
		"type":       "event",
		"event_type": eventType,
		"domain":     domain,
		"page_url":   pagePath,
		"page_title": pageTitle,
		"active":     atomic.LoadInt64(&h.activeNow),
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
	})

	for ch := range h.sseClients {
		select {
		case ch <- msg:
		default:
		}
	}
}

// HandleGetIdentifiedVisitors returns visitors with known subscriber identity.
// GET /api/mailing/site-pixel/visitors?range=24h&domain=discountblog.com
func (h *SiteEventsHandler) HandleGetIdentifiedVisitors(w http.ResponseWriter, r *http.Request) {
	timeRange := r.URL.Query().Get("range")
	if timeRange == "" {
		timeRange = "24h"
	}
	domain := r.URL.Query().Get("domain")

	var interval string
	switch timeRange {
	case "1h":
		interval = "1 hour"
	case "24h":
		interval = "24 hours"
	case "7d":
		interval = "7 days"
	case "30d":
		interval = "30 days"
	default:
		interval = "24 hours"
	}

	type identifiedVisitor struct {
		Email     string `json:"email"`
		SubID     string `json:"subscriber_id"`
		PageURL   string `json:"page_url"`
		PageTitle string `json:"page_title"`
		Domain    string `json:"domain"`
		IP        string `json:"ip_address"`
		UserAgent string `json:"user_agent"`
		EventAt   string `json:"event_at"`
		EventType string `json:"event_type"`
	}

	var visitors []identifiedVisitor
	query := `
		SELECT COALESCE(subscriber_email,''),
		       COALESCE(subscriber_id::text,''),
		       COALESCE(metadata->>'page_url',''),
		       COALESCE(metadata->>'page_title',''),
		       COALESCE(metadata->>'domain',''),
		       COALESCE(ip_address,''),
		       COALESCE(user_agent,''),
		       event_at::text,
		       event_type
		FROM subscriber_events
		WHERE source IN ('site','site_beacon')
		  AND subscriber_email IS NOT NULL AND subscriber_email != ''
		  AND event_at > NOW() - $1::interval`

	args := []interface{}{interval}
	if domain != "" {
		query += ` AND metadata->>'domain' = $2`
		args = append(args, domain)
	}
	query += ` ORDER BY event_at DESC LIMIT 200`

	rows, err := h.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		log.Printf("[SitePixel] visitors query error: %v", err)
		respondJSON(w, http.StatusOK, map[string]interface{}{"visitors": []identifiedVisitor{}})
		return
	}
	defer rows.Close()

	for rows.Next() {
		var v identifiedVisitor
		rows.Scan(&v.Email, &v.SubID, &v.PageURL, &v.PageTitle, &v.Domain, &v.IP, &v.UserAgent, &v.EventAt, &v.EventType)
		visitors = append(visitors, v)
	}
	if visitors == nil {
		visitors = []identifiedVisitor{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"visitors":   visitors,
		"total":      len(visitors),
		"time_range": timeRange,
		"domain":     domain,
	})
}

// HandleGetTrackedDomains returns domains that have been sending pixel events.
func (h *SiteEventsHandler) HandleGetTrackedDomains(w http.ResponseWriter, r *http.Request) {
	type domainInfo struct {
		Domain     string `json:"domain"`
		Pageviews  int    `json:"pageviews_24h"`
		Visitors   int    `json:"unique_visitors_24h"`
		LastSeen   string `json:"last_seen"`
	}
	var domains []domainInfo
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT metadata->>'domain' AS domain,
		       COUNT(*) AS pvs,
		       COUNT(DISTINCT email_hash) AS uvs,
		       MAX(event_at)::text AS last_seen
		FROM subscriber_events
		WHERE source IN ('site','site_beacon')
		  AND event_type = 'page_view'
		  AND event_at > NOW() - INTERVAL '24 hours'
		  AND metadata->>'domain' IS NOT NULL
		  AND metadata->>'domain' != ''
		GROUP BY domain ORDER BY pvs DESC
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var d domainInfo
			rows.Scan(&d.Domain, &d.Pageviews, &d.Visitors, &d.LastSeen)
			domains = append(domains, d)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"domains": domains})
}

func generateTrackingSnippet(domain, apiBase string) string {
	return fmt.Sprintf(`<!-- Jarvis Site Intelligence Pixel — %s -->
<script>
(function(){
  var J=window.__jarvis=window.__jarvis||{};
  if(J.loaded)return;J.loaded=true;
  var API='%s/api/v1/site-events';
  var BEACON='%s/api/v1/site-events/beacon';
  var D='%s';
  var CK='_jv_track';
  function gp(n){return new URLSearchParams(location.search).get(n)}
  var urlSid=gp('sid'),urlEid=gp('eid'),urlCid=gp('cid');
  if(urlSid&&urlEid&&urlCid){document.cookie=CK+'='+urlEid+'|'+urlCid+'|'+urlSid+';path=/;max-age=2592000;SameSite=Lax'}
  function getTrack(){var c=document.cookie.split('; ').find(function(c){return c.startsWith(CK+'=')});if(!c)return null;var p=c.split('=')[1].split('|');return p.length>=3?{eid:p[0],cid:p[1],sid:p[2]}:null}
  function uid(){try{var k='_jv_uid',v=localStorage.getItem(k);if(v)return v;v='jv_'+Math.random().toString(36).substr(2,12)+Date.now().toString(36);localStorage.setItem(k,v);return v}catch(e){return 'jv_'+Math.random().toString(36).substr(2,12)}}
  function send(evt,meta){
    var t=getTrack()||{};
    var data=JSON.stringify({eid:t.eid||uid(),cid:t.cid||'',sid:t.sid||'',event_type:evt,page_url:location.pathname+location.search,page_title:document.title,referrer:document.referrer,domain:D,metadata:meta||{}});
    if(navigator.sendBeacon){navigator.sendBeacon(API,new Blob([data],{type:'application/json'}))}
    else{var x=new XMLHttpRequest();x.open('POST',API,true);x.setRequestHeader('Content-Type','application/json');x.send(data)}
  }
  send('page_view');
  var startTime=Date.now();
  function sendEngagement(){
    var dur=Math.round((Date.now()-startTime)/1000);
    var scrollPct=Math.round(100*window.scrollY/(Math.max(document.body.scrollHeight-window.innerHeight,1)));
    send('engagement',{time_on_page:dur,scroll_depth:scrollPct});
  }
  if(document.visibilityState!==undefined){document.addEventListener('visibilitychange',function(){if(document.visibilityState==='hidden')sendEngagement()})}
  window.addEventListener('beforeunload',sendEngagement);
  var t=getTrack()||{};
  var img=new Image();img.src=BEACON+'?eid='+encodeURIComponent(t.eid||uid())+'&sid='+encodeURIComponent(t.sid||'')+'&cid='+encodeURIComponent(t.cid||'')+'&d='+encodeURIComponent(D)+'&p='+encodeURIComponent(location.pathname)+'&t='+encodeURIComponent(document.title);
})();
</script>`, domain, apiBase, apiBase, domain)
}

var transparentGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00,
	0x80, 0x00, 0x00, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x21,
	0xf9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00,
	0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44,
	0x01, 0x00, 0x3b,
}

func siteEventHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

var allowedRefererDomains = []string{
	"getmecoupons.net", "discountblog.com", "quizfiesta.com",
	"historythinking.com", "myownhealth.net",
	"localhost", "projectjarvis.io",
}

func isAllowedReferer(referer string) bool {
	lower := strings.ToLower(referer)
	for _, d := range allowedRefererDomains {
		if strings.Contains(lower, d) {
			return true
		}
	}
	return false
}

var botKeywords = []string{"bot", "crawler", "spider", "headless", "phantom", "wget", "curl", "python-requests"}

func isBot(ua string) bool {
	lower := strings.ToLower(ua)
	for _, kw := range botKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func metaJSON(meta map[string]interface{}) []byte {
	b, err := json.Marshal(meta)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func safeDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// HandleISPReconciliation compares email engagement metrics with first-party
// site visit data by ISP to identify metric suppression.
// GET /api/mailing/site-pixel/isp-reconciliation?range=7d
func (h *SiteEventsHandler) HandleISPReconciliation(w http.ResponseWriter, r *http.Request) {
	timeRange := r.URL.Query().Get("range")
	if timeRange == "" {
		timeRange = "7d"
	}
	var interval string
	switch timeRange {
	case "1d":
		interval = "1 day"
	case "7d":
		interval = "7 days"
	case "30d":
		interval = "30 days"
	default:
		interval = "7 days"
	}

	query := `
	WITH email_engagement AS (
		SELECT
			CASE
				WHEN s.email ILIKE '%gmail%'     THEN 'Gmail'
				WHEN s.email ILIKE '%yahoo%' OR s.email ILIKE '%ymail%' THEN 'Yahoo'
				WHEN s.email ILIKE '%outlook%' OR s.email ILIKE '%hotmail%' OR s.email ILIKE '%live.com%' THEN 'Microsoft'
				WHEN s.email ILIKE '%aol%'       THEN 'AOL'
				WHEN s.email ILIKE '%icloud%' OR s.email ILIKE '%me.com%' OR s.email ILIKE '%mac.com%' THEN 'Apple'
				WHEN s.email ILIKE '%comcast%'   THEN 'Comcast'
				ELSE 'Other'
			END AS isp,
			COUNT(DISTINCT s.id)                                              AS total_subscribers,
			COUNT(DISTINCT CASE WHEN te.event_type = 'sent'      THEN s.id END) AS email_sent,
			COUNT(DISTINCT CASE WHEN te.event_type = 'opened'    THEN s.id END) AS email_opened,
			COUNT(DISTINCT CASE WHEN te.event_type = 'clicked'   THEN s.id END) AS email_clicked
		FROM mailing_subscribers s
		LEFT JOIN mailing_tracking_events te ON te.subscriber_id = s.id
			AND te.event_at > NOW() - $1::interval
		WHERE s.status = 'active'
		GROUP BY 1
	),
	site_visits AS (
		SELECT
			CASE
				WHEN se.subscriber_email ILIKE '%gmail%'     THEN 'Gmail'
				WHEN se.subscriber_email ILIKE '%yahoo%' OR se.subscriber_email ILIKE '%ymail%' THEN 'Yahoo'
				WHEN se.subscriber_email ILIKE '%outlook%' OR se.subscriber_email ILIKE '%hotmail%' OR se.subscriber_email ILIKE '%live.com%' THEN 'Microsoft'
				WHEN se.subscriber_email ILIKE '%aol%'       THEN 'AOL'
				WHEN se.subscriber_email ILIKE '%icloud%' OR se.subscriber_email ILIKE '%me.com%' OR se.subscriber_email ILIKE '%mac.com%' THEN 'Apple'
				WHEN se.subscriber_email ILIKE '%comcast%'   THEN 'Comcast'
				ELSE 'Other'
			END AS isp,
			COUNT(DISTINCT se.subscriber_id) AS site_unique_visitors,
			COUNT(*) AS site_total_pageviews
		FROM subscriber_events se
		WHERE se.source IN ('site','site_beacon')
			AND se.event_type = 'page_view'
			AND se.subscriber_email IS NOT NULL AND se.subscriber_email != ''
			AND se.event_at > NOW() - $1::interval
		GROUP BY 1
	)
	SELECT
		COALESCE(e.isp, v.isp)                          AS isp,
		COALESCE(e.total_subscribers, 0)                 AS total_subscribers,
		COALESCE(e.email_sent, 0)                        AS email_sent,
		COALESCE(e.email_opened, 0)                      AS email_opened,
		COALESCE(e.email_clicked, 0)                     AS email_clicked,
		COALESCE(v.site_unique_visitors, 0)              AS site_unique_visitors,
		COALESCE(v.site_total_pageviews, 0)              AS site_total_pageviews,
		CASE WHEN COALESCE(e.email_clicked, 0) > 0
			THEN ROUND(COALESCE(v.site_unique_visitors,0)::numeric / e.email_clicked * 100, 1)
			ELSE 0
		END                                              AS click_validation_pct,
		GREATEST(COALESCE(v.site_unique_visitors,0) - COALESCE(e.email_clicked,0), 0) AS ghost_visitors
	FROM email_engagement e
	FULL OUTER JOIN site_visits v ON e.isp = v.isp
	ORDER BY COALESCE(e.total_subscribers, 0) DESC
	`

	rows, err := h.db.QueryContext(r.Context(), query, interval)
	if err != nil {
		log.Printf("[ISPRecon] query error: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	type ispRow struct {
		ISP                string  `json:"isp"`
		TotalSubscribers   int     `json:"total_subscribers"`
		EmailSent          int     `json:"email_sent"`
		EmailOpened        int     `json:"email_opened"`
		EmailClicked       int     `json:"email_clicked"`
		SiteUniqueVisitors int     `json:"site_unique_visitors"`
		SiteTotalPageviews int     `json:"site_total_pageviews"`
		ClickValidationPct float64 `json:"click_validation_pct"`
		GhostVisitors      int     `json:"ghost_visitors"`
	}

	var results []ispRow
	for rows.Next() {
		var r ispRow
		rows.Scan(
			&r.ISP, &r.TotalSubscribers, &r.EmailSent, &r.EmailOpened,
			&r.EmailClicked, &r.SiteUniqueVisitors, &r.SiteTotalPageviews,
			&r.ClickValidationPct, &r.GhostVisitors,
		)
		results = append(results, r)
	}
	if results == nil {
		results = []ispRow{}
	}

	var totalGhost, totalSiteVisitors, totalEmailClicked int
	for _, r := range results {
		totalGhost += r.GhostVisitors
		totalSiteVisitors += r.SiteUniqueVisitors
		totalEmailClicked += r.EmailClicked
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"isps":                results,
		"time_range":          timeRange,
		"total_ghost_visitors": totalGhost,
		"total_site_visitors":  totalSiteVisitors,
		"total_email_clicked":  totalEmailClicked,
	})
}

// HandleGhostVisitors returns subscriber-level detail for ghost visitors:
// subscribers with site activity but no email clicks recorded by the ISP.
// Uses 45-day click window vs 30-day site visit window to prevent boundary false positives.
// GET /api/mailing/site-pixel/ghost-visitors?limit=100
func (h *SiteEventsHandler) HandleGhostVisitors(w http.ResponseWriter, r *http.Request) {
	limitInt := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 500 {
		limitInt = v
	}

	query := `
	SELECT
		se.subscriber_email,
		se.subscriber_id::text,
		COUNT(*)                                 AS site_pageviews,
		MAX(se.event_at)                         AS last_visit_at,
		COALESCE(MAX(se.metadata->>'domain'),'') AS last_domain,
		COALESCE(MAX(se.metadata->>'page_url'),'') AS last_page
	FROM subscriber_events se
	WHERE se.source IN ('site','site_beacon')
		AND se.event_type = 'page_view'
		AND se.subscriber_email IS NOT NULL AND se.subscriber_email != ''
		AND se.subscriber_id IS NOT NULL
		AND se.event_at > NOW() - INTERVAL '30 days'
		AND NOT EXISTS (
			SELECT 1 FROM mailing_tracking_events te
			WHERE te.subscriber_id = se.subscriber_id
				AND te.event_type = 'clicked'
				AND te.event_at > NOW() - INTERVAL '45 days'
		)
	GROUP BY se.subscriber_email, se.subscriber_id
	ORDER BY site_pageviews DESC
	LIMIT $1
	`

	rows, err := h.db.QueryContext(r.Context(), query, limitInt)
	if err != nil {
		log.Printf("[GhostVisitors] query error: %v", err)
		respondJSON(w, http.StatusOK, map[string]interface{}{"ghost_visitors": []interface{}{}, "total": 0})
		return
	}
	defer rows.Close()

	type ghostRow struct {
		Email        string `json:"email"`
		SubscriberID string `json:"subscriber_id"`
		SiteViews    int    `json:"site_pageviews"`
		LastVisitAt  string `json:"last_visit_at"`
		LastDomain   string `json:"last_domain"`
		LastPage     string `json:"last_page"`
	}

	var results []ghostRow
	for rows.Next() {
		var g ghostRow
		var visitAt time.Time
		rows.Scan(&g.Email, &g.SubscriberID, &g.SiteViews, &visitAt, &g.LastDomain, &g.LastPage)
		g.LastVisitAt = visitAt.Format(time.RFC3339)
		results = append(results, g)
	}
	if results == nil {
		results = []ghostRow{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ghost_visitors": results,
		"total":          len(results),
	})
}
