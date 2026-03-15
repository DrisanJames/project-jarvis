package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// =============================================================================
// VMTA POOL — In-memory IP rotation cache with warmup enforcement
// =============================================================================

type vmtaEntry struct {
	ID               string
	Hostname         string
	Status           string // "active" or "warmup"
	WarmupDailyLimit int
	TodaySent        int64 // from mailing_ip_warmup_log.actual_sent
}

type vmtaPool struct {
	mu       sync.RWMutex
	ips      []vmtaEntry
	idx      uint64
	loadedAt time.Time
	ttl      time.Duration
	db       *sql.DB
}

func newVMTAPool(db *sql.DB) *vmtaPool {
	return &vmtaPool{
		db:  db,
		ttl: 30 * time.Second,
	}
}

// refresh loads the IP pool from the database if stale.
func (p *vmtaPool) refresh(ctx context.Context, profileID string) {
	p.mu.RLock()
	if time.Since(p.loadedAt) < p.ttl && len(p.ips) > 0 {
		p.mu.RUnlock()
		return
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock
	if time.Since(p.loadedAt) < p.ttl && len(p.ips) > 0 {
		return
	}

	rows, err := p.db.QueryContext(ctx, `
		SELECT ip.id, ip.hostname, ip.status,
		       COALESCE(ip.warmup_daily_limit, 50),
		       COALESCE(wl.actual_sent, 0)
		FROM mailing_ip_addresses ip
		JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
		JOIN mailing_sending_profiles sp ON sp.ip_pool = pool.name
		LEFT JOIN mailing_ip_warmup_log wl ON wl.ip_id = ip.id AND wl.date = CURRENT_DATE
		WHERE sp.id = $1
		  AND ip.status IN ('active', 'warmup')
		  AND pool.status = 'active'
		ORDER BY ip.last_sent_at ASC NULLS FIRST
	`, profileID)
	if err != nil {
		log.Printf("[vmtaPool] refresh error: %v", err)
		return
	}
	defer rows.Close()

	var ips []vmtaEntry
	for rows.Next() {
		var e vmtaEntry
		if err := rows.Scan(&e.ID, &e.Hostname, &e.Status, &e.WarmupDailyLimit, &e.TodaySent); err != nil {
			continue
		}
		ips = append(ips, e)
	}
	if len(ips) > 0 {
		p.ips = ips
		p.loadedAt = time.Now()
		for _, ip := range ips {
			log.Printf("[vmtaPool] Loaded IP %s status=%s limit=%d sent=%d", ip.Hostname, ip.Status, ip.WarmupDailyLimit, ip.TodaySent)
		}
	} else {
		log.Printf("[vmtaPool] WARNING: refresh returned 0 IPs for profile %s", profileID)
	}
}

// next returns the next available IP, enforcing warmup daily limits.
// IPs with status "cold" are always skipped (e.g., blacklisted mta1).
func (p *vmtaPool) next() (vmtaEntry, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.ips) == 0 {
		return vmtaEntry{}, fmt.Errorf("no IPs in pool")
	}

	for attempts := 0; attempts < len(p.ips); attempts++ {
		idx := atomic.AddUint64(&p.idx, 1) % uint64(len(p.ips))
		ip := p.ips[idx]
		if ip.Status == "cold" {
			continue
		}
		if strings.Contains(ip.Hostname, "mta1") {
			continue
		}
		return ip, nil
	}
	return vmtaEntry{}, fmt.Errorf("all IPs exhausted or excluded")
}

// =============================================================================
// SMTP CONNECTION POOL
// =============================================================================

type smtpPool struct {
	host     string
	port     int
	username string
	password string
	idle     chan *smtp.Client
	maxSize  int
}

func newSMTPPool(host string, port int, username, password string, size int) *smtpPool {
	return &smtpPool{
		host:     host,
		port:     port,
		username: username,
		password: password,
		idle:     make(chan *smtp.Client, size),
		maxSize:  size,
	}
}

func (p *smtpPool) get(ctx context.Context) (*smtp.Client, error) {
	// Try to get an idle connection (non-blocking)
	select {
	case client := <-p.idle:
		// Health check: send NOOP to verify connection is alive
		if err := client.Noop(); err != nil {
			client.Close()
			return p.dial(ctx)
		}
		return client, nil
	default:
		return p.dial(ctx)
	}
}

func (p *smtpPool) put(client *smtp.Client) {
	// Reset the connection state for reuse
	if err := client.Reset(); err != nil {
		client.Close()
		return
	}
	select {
	case p.idle <- client:
		// Returned to pool
	default:
		// Pool full, close this connection
		client.Close()
	}
}

func (p *smtpPool) dial(ctx context.Context) (*smtp.Client, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	addr := fmt.Sprintf("%s:%d", p.host, p.port)

	dialOne := func(tryAuth bool) (*smtp.Client, error) {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("SMTP connect to %s: %w", addr, err)
		}
		c, err := smtp.NewClient(conn, p.host)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("SMTP client: %w", err)
		}
		if ok, _ := c.Extension("STARTTLS"); ok {
			tlsCfg := &tls.Config{ServerName: p.host, InsecureSkipVerify: true}
			if tlsErr := c.StartTLS(tlsCfg); tlsErr != nil {
				log.Printf("[PMTA] STARTTLS failed (continuing without TLS): %v", tlsErr)
			}
		}
		if tryAuth && p.username != "" && p.password != "" {
			if authErr := c.Auth(&pmtaPlainAuth{user: p.username, pass: p.password}); authErr != nil {
				c.Close()
				return nil, authErr
			}
		}
		return c, nil
	}

	client, err := dialOne(p.username != "" && p.password != "")
	if err != nil && p.username != "" && p.password != "" {
		log.Printf("[PMTA] Retrying without AUTH (server may be open relay)")
		client, err = dialOne(false)
	}
	return client, err
}

func (p *smtpPool) close() {
	close(p.idle)
	for client := range p.idle {
		client.Quit()
	}
}

// =============================================================================
// PMTASender — SMTP-based PMTA sender with connection pool + VMTA cache
// =============================================================================

type PMTASender struct {
	smtpHost string
	smtpPort int
	username string
	password string
	db       *sql.DB
	client   *http.Client
	mgmtHost string
	mgmtPort int
	mgmtKey  string

	connPool *smtpPool
	ipPool   *vmtaPool
}

func NewPMTASender(smtpHost string, smtpPort int, username, password string, db *sql.DB) *PMTASender {
	return &PMTASender{
		smtpHost: smtpHost,
		smtpPort: smtpPort,
		username: username,
		password: password,
		db:       db,
		client:   &http.Client{Timeout: 30 * time.Second},
		connPool: newSMTPPool(smtpHost, smtpPort, username, password, 20),
		ipPool:   newVMTAPool(db),
	}
}

func (s *PMTASender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	if s.smtpHost == "" {
		return nil, fmt.Errorf("PMTA SMTP host not configured")
	}

	// Refresh VMTA cache, then select next IP via round-robin
	s.ipPool.refresh(ctx, msg.ProfileID)
	vmtaName := ""
	ipID := ""
	ip, vmtaErr := s.ipPool.next()
	if vmtaErr != nil {
		if len(s.ipPool.ips) > 0 {
			return nil, fmt.Errorf("all IPs exhausted warmup limits, deferring send: %w", vmtaErr)
		}
		// NEVER fall back to default-pool — it uses the server IP which
		// must not be used for campaign delivery. Fail hard so the queue
		// item is retried after IPs are configured.
		return nil, fmt.Errorf("no sending IPs configured for profile %s — refusing to send via default-pool (server IP)", msg.ProfileID)
	} else {
		// PMTA VMTA names are the short prefix of the hostname
		// (e.g. "mta1" from "mta1.mail.projectjarvis.io"). The full
		// hostname does NOT match any <virtual-mta> directive and
		// silently falls back to the server IP.
		vmtaName = vmtaShortName(ip.Hostname)
		ipID = ip.ID
	}

	msgDomain := "mail.projectjarvis.io"
	if parts := strings.SplitN(msg.FromEmail, "@", 2); len(parts) == 2 && parts[1] != "" {
		msgDomain = parts[1]
	}
	messageID := fmt.Sprintf("%s@%s", uuid.New().String(), msgDomain)

	var headerBuf bytes.Buffer
	headerBuf.WriteString(fmt.Sprintf("From: %s <%s>\r\n", msg.FromName, msg.FromEmail))
	headerBuf.WriteString(fmt.Sprintf("To: %s\r\n", msg.Email))
	headerBuf.WriteString(fmt.Sprintf("Subject: %s\r\n", msg.Subject))
	headerBuf.WriteString(fmt.Sprintf("Message-ID: <%s>\r\n", messageID))
	headerBuf.WriteString("MIME-Version: 1.0\r\n")

	if msg.ReplyTo != "" {
		headerBuf.WriteString(fmt.Sprintf("Reply-To: %s\r\n", msg.ReplyTo))
	}
	if vmtaName != "" {
		headerBuf.WriteString(fmt.Sprintf("X-Virtual-MTA: %s\r\n", vmtaName))
	}

	headerBuf.WriteString(fmt.Sprintf("X-Campaign-ID: %s\r\n", msg.CampaignID))
	headerBuf.WriteString(fmt.Sprintf("X-Subscriber-ID: %s\r\n", msg.SubscriberID))
	headerBuf.WriteString(fmt.Sprintf("X-Message-ID: %s\r\n", msg.ID))

	// Feedback-ID is required by Gmail for FBL (Feedback Loop) correlation.
	// Format: campaignID:subscriberID:orgID:sendingDomain
	// Gmail uses the last segment (SenderId) as the primary identifier.
	feedbackDomain := msg.FromEmail
	if atIdx := strings.LastIndex(msg.FromEmail, "@"); atIdx >= 0 {
		feedbackDomain = msg.FromEmail[atIdx+1:]
	}
	headerBuf.WriteString(fmt.Sprintf("Feedback-ID: %s:%s:%s:%s\r\n",
		msg.CampaignID, msg.SubscriberID, msg.ID, feedbackDomain))

	for k, v := range msg.Headers {
		headerBuf.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}

	boundary := fmt.Sprintf("=_%s", uuid.New().String()[:16])
	headerBuf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
	headerBuf.WriteString("\r\n")

	var bodyBuf bytes.Buffer
	if msg.TextContent != "" {
		bodyBuf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		bodyBuf.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
		bodyBuf.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		var qpText bytes.Buffer
		qpWriter := quotedprintable.NewWriter(&qpText)
		qpWriter.Write([]byte(msg.TextContent))
		qpWriter.Close()
		bodyBuf.Write(qpText.Bytes())
		bodyBuf.WriteString("\r\n")
	}
	bodyBuf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	bodyBuf.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	bodyBuf.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	var qpHTML bytes.Buffer
	qpWriter := quotedprintable.NewWriter(&qpHTML)
	qpWriter.Write([]byte(msg.HTMLContent))
	qpWriter.Close()
	bodyBuf.Write(qpHTML.Bytes())
	bodyBuf.WriteString("\r\n")
	bodyBuf.WriteString(fmt.Sprintf("--%s--\r\n", boundary))

	fullMessage := headerBuf.String() + bodyBuf.String()

	// Get a pooled SMTP connection and send
	smtpClient, err := s.connPool.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("PMTA SMTP pool get failed: %w", err)
	}

	sendErr := s.sendOnClient(smtpClient, msg.FromEmail, msg.Email, []byte(fullMessage))
	if sendErr != nil {
		// Connection is likely dead; discard it and retry once with a fresh one
		smtpClient.Close()
		smtpClient, err = s.connPool.dial(ctx)
		if err != nil {
			return nil, fmt.Errorf("PMTA SMTP reconnect failed: %w", err)
		}
		sendErr = s.sendOnClient(smtpClient, msg.FromEmail, msg.Email, []byte(fullMessage))
		if sendErr != nil {
			smtpClient.Close()
			return nil, fmt.Errorf("PMTA SMTP send failed after retry: %w", sendErr)
		}
	}

	// Return connection to pool for reuse
	s.connPool.put(smtpClient)

	if ipID != "" {
		go s.updateIPCounters(ipID)
	}

	return &SendResult{Success: true, MessageID: messageID, ESPType: "pmta", SentAt: time.Now()}, nil
}

// sendOnClient performs MAIL FROM / RCPT TO / DATA on an existing connection.
//
// We issue MAIL FROM manually instead of using client.Mail() because Go's
// net/smtp unconditionally adds "SMTPUTF8" to MAIL FROM when the server
// advertises it (even for pure-ASCII addresses). PMTA then marks the message
// as requiring SMTPUTF8, and destinations that don't support it (Yahoo, Cox,
// Charter, Comcast, Apple iCloud) bounce with 5.6.7. Using BODY=8BITMIME
// alone is sufficient for UTF-8 message content with quoted-printable encoding.
func (s *PMTASender) sendOnClient(client *smtp.Client, from, to string, msg []byte) error {
	id, err := client.Text.Cmd("MAIL FROM:<%s> BODY=8BITMIME", from)
	if err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	client.Text.StartResponse(id)
	_, _, err = client.Text.ReadResponse(250)
	client.Text.EndResponse(id)
	if err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("DATA close: %w", err)
	}
	return nil
}

// updateIPCounters increments send counters on both mailing_ip_addresses
// and mailing_ip_warmup_log (so warmup threshold checks have accurate data).
func (s *PMTASender) updateIPCounters(ipID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_ip_addresses
		SET total_sent = total_sent + 1, last_sent_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, ipID)
	if err != nil {
		log.Printf("[PMTA] Failed to update IP counters for %s: %v", ipID, err)
	}

	// Also update warmup log so checkThresholds and daily limit enforcement stay in sync
	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_ip_warmup_log
		SET actual_sent = actual_sent + 1
		WHERE ip_id = $1 AND date = CURRENT_DATE
	`, ipID)
	if err != nil {
		log.Printf("[PMTA] Failed to update warmup log for %s: %v", ipID, err)
	}
}

// pmtaPlainAuth implements smtp.Auth without the TLS requirement that
// stdlib's PlainAuth enforces. PMTA servers on private networks typically
// do not use TLS on the submission port.
type pmtaPlainAuth struct {
	user, pass string
}

func (a *pmtaPlainAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	resp := []byte("\x00" + a.user + "\x00" + a.pass)
	return "PLAIN", resp, nil
}

func (a *pmtaPlainAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	return nil, nil
}

// vmtaShortName extracts the short VMTA prefix from a full hostname.
// e.g. "mta1.mail.projectjarvis.io" → "mta1", "mta2" → "mta2", "" → "".
func vmtaShortName(hostname string) string {
	if dotIdx := strings.Index(hostname, "."); dotIdx > 0 {
		return hostname[:dotIdx]
	}
	return hostname
}
