package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"time"

	"github.com/google/uuid"
)

// PMTASender sends emails via a PowerMTA server over SMTP.
// Uses X-Virtual-MTA headers to route messages through specific IPs for
// domain-level reputation isolation.
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
}

// NewPMTASender creates a PMTA sender configured with SMTP credentials.
func NewPMTASender(smtpHost string, smtpPort int, username, password string, db *sql.DB) *PMTASender {
	return &PMTASender{
		smtpHost: smtpHost,
		smtpPort: smtpPort,
		username: username,
		password: password,
		db:       db,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Send delivers a single email through PMTA, selecting a VMTA from the
// sending profile's IP pool for round-robin rotation.
func (s *PMTASender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	if s.smtpHost == "" {
		return nil, fmt.Errorf("PMTA SMTP host not configured")
	}

	vmtaName, ipID, err := s.selectVMTA(ctx, msg.ProfileID)
	if err != nil {
		log.Printf("[PMTA] VMTA selection failed, sending without routing: %v", err)
	}

	messageID := fmt.Sprintf("%s@pmta", uuid.New().String())

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
		bodyBuf.WriteString(msg.TextContent)
		bodyBuf.WriteString("\r\n")
	}
	bodyBuf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	bodyBuf.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	bodyBuf.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	bodyBuf.WriteString(msg.HTMLContent)
	bodyBuf.WriteString("\r\n")
	bodyBuf.WriteString(fmt.Sprintf("--%s--\r\n", boundary))

	fullMessage := headerBuf.String() + bodyBuf.String()
	addr := fmt.Sprintf("%s:%d", s.smtpHost, s.smtpPort)
	if err := s.sendSMTP(ctx, addr, msg.FromEmail, msg.Email, []byte(fullMessage)); err != nil {
		return nil, fmt.Errorf("PMTA SMTP send failed: %w", err)
	}

	if ipID != "" {
		go s.updateIPCounters(ipID)
	}

	log.Printf("[PMTA] Sent to %s (id: %s, vmta: %s)", msg.Email, messageID, vmtaName)
	return &SendResult{Success: true, MessageID: messageID, ESPType: "pmta", SentAt: time.Now()}, nil
}

// selectVMTA picks the least-recently-used IP from the sending profile's pool.
func (s *PMTASender) selectVMTA(ctx context.Context, profileID string) (string, string, error) {
	if s.db == nil || profileID == "" {
		return "", "", nil
	}
	var vmtaHostname, ipID string
	err := s.db.QueryRowContext(ctx, `
		SELECT ip.id, ip.hostname
		FROM mailing_ip_addresses ip
		JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
		JOIN mailing_sending_profiles sp ON sp.ip_pool = pool.name
		WHERE sp.id = $1
		  AND ip.status IN ('active', 'warmup')
		  AND pool.status = 'active'
		ORDER BY ip.last_sent_at ASC NULLS FIRST
		LIMIT 1
	`, profileID).Scan(&ipID, &vmtaHostname)
	if err != nil {
		return "", "", fmt.Errorf("no available VMTA for profile %s: %w", profileID, err)
	}
	return vmtaHostname, ipID, nil
}

// updateIPCounters increments the send counter and updates last_sent_at.
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
}

// sendSMTP performs the raw SMTP transaction with the PMTA server.
// If AUTH fails (common when PMTA has no inbound TLS), it reconnects
// without AUTH since the relay is typically open.
func (s *PMTASender) sendSMTP(ctx context.Context, addr, from, to string, msg []byte) error {
	dialer := &net.Dialer{Timeout: 30 * time.Second}

	dialAndSetup := func(tryAuth bool) (*smtp.Client, error) {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("SMTP connect to %s: %w", addr, err)
		}
		c, err := smtp.NewClient(conn, s.smtpHost)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("SMTP client: %w", err)
		}
		if ok, _ := c.Extension("STARTTLS"); ok {
			tlsCfg := &tls.Config{ServerName: s.smtpHost, InsecureSkipVerify: true}
			if tlsErr := c.StartTLS(tlsCfg); tlsErr != nil {
				log.Printf("[PMTA] STARTTLS failed (continuing without TLS): %v", tlsErr)
			}
		}
		if tryAuth && s.username != "" && s.password != "" {
			if authErr := c.Auth(&pmtaPlainAuth{user: s.username, pass: s.password}); authErr != nil {
				log.Printf("[PMTA] AUTH failed: %v", authErr)
				c.Close()
				return nil, authErr
			}
		}
		return c, nil
	}

	client, err := dialAndSetup(s.username != "" && s.password != "")
	if err != nil && s.username != "" && s.password != "" {
		log.Printf("[PMTA] Retrying without AUTH (server may be open relay)")
		client, err = dialAndSetup(false)
	}
	if err != nil {
		return fmt.Errorf("SMTP setup: %w", err)
	}
	defer client.Close()

	if err := client.Mail(from); err != nil {
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
	return client.Quit()
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
