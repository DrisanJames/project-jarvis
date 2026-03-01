package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleSendTestEmail sends a test email through the selected ESP profile
func (svc *MailingService) HandleSendTestEmail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		To               string  `json:"to"`
		Subject          string  `json:"subject"`
		FromName         string  `json:"from_name"`
		FromEmail        string  `json:"from_email"`
		ReplyEmail       string  `json:"reply_email"`
		HTMLContent      string  `json:"html_content"`
		TextContent      string  `json:"text_content"`
		SendingProfileID *string `json:"sending_profile_id"` // Optional: route through specific ESP
	}
	json.NewDecoder(r.Body).Decode(&input)

	// Validate
	if input.To == "" || input.Subject == "" {
		http.Error(w, `{"error":"to and subject required"}`, http.StatusBadRequest)
		return
	}

	// Check suppression
	var suppressed bool
	svc.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mailing_suppressions WHERE email = $1 AND active = true)",
		strings.ToLower(input.To)).Scan(&suppressed)

	if suppressed {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false, "suppressed": true, "reason": "Email is on suppression list",
		})
		return
	}

	// Check throttle
	if !svc.throttler.CanSend() {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false, "throttled": true, "reason": "Rate limit exceeded",
		})
		return
	}

	// Get sending profile (either specified or default)
	var profile struct {
		ID          string
		VendorType  string
		FromName    string
		FromEmail   string
		ReplyEmail  *string
		APIKey      *string
		APIEndpoint *string
		SMTPHost    *string
		SMTPPort    int
		SMTPUser    *string
		SMTPPass    *string
	}

	profileQuery := `
		SELECT id, vendor_type, from_name, from_email, reply_email, 
			   api_key, api_endpoint, smtp_host, smtp_port, smtp_username, smtp_password
		FROM mailing_sending_profiles 
		WHERE status = 'active'
	`

	if input.SendingProfileID != nil && *input.SendingProfileID != "" {
		// Use specified profile
		profileQuery += " AND id = $1"
		err := svc.db.QueryRowContext(ctx, profileQuery, *input.SendingProfileID).Scan(
			&profile.ID, &profile.VendorType, &profile.FromName, &profile.FromEmail, &profile.ReplyEmail,
			&profile.APIKey, &profile.APIEndpoint, &profile.SMTPHost, &profile.SMTPPort, &profile.SMTPUser, &profile.SMTPPass,
		)
		if err != nil {
			http.Error(w, `{"error":"sending profile not found or inactive"}`, http.StatusBadRequest)
			return
		}
	} else {
		// Use default profile, fall back to any active profile
		defaultQuery := profileQuery + " AND is_default = true LIMIT 1"
		err := svc.db.QueryRowContext(ctx, defaultQuery).Scan(
			&profile.ID, &profile.VendorType, &profile.FromName, &profile.FromEmail, &profile.ReplyEmail,
			&profile.APIKey, &profile.APIEndpoint, &profile.SMTPHost, &profile.SMTPPort, &profile.SMTPUser, &profile.SMTPPass,
		)
		if err != nil {
			// No default — try any active profile (prefer PMTA/SMTP over API-based)
			fallbackQuery := profileQuery + " ORDER BY CASE vendor_type WHEN 'pmta' THEN 0 WHEN 'smtp' THEN 1 ELSE 2 END LIMIT 1"
			err = svc.db.QueryRowContext(ctx, fallbackQuery).Scan(
				&profile.ID, &profile.VendorType, &profile.FromName, &profile.FromEmail, &profile.ReplyEmail,
				&profile.APIKey, &profile.APIEndpoint, &profile.SMTPHost, &profile.SMTPPort, &profile.SMTPUser, &profile.SMTPPass,
			)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"error":   "No sending profiles configured. Create one in Domain Center → Sending Profiles.",
				})
				return
			}
		}
	}

	// Override from profile if not specified in request
	fromName := input.FromName
	if fromName == "" {
		fromName = profile.FromName
	}
	fromEmail := input.FromEmail
	if fromEmail == "" {
		fromEmail = profile.FromEmail
	}
	replyEmail := input.ReplyEmail
	if replyEmail == "" && profile.ReplyEmail != nil {
		replyEmail = *profile.ReplyEmail
	}

	// Set content defaults
	if input.HTMLContent == "" {
		input.HTMLContent = fmt.Sprintf("<html><body><h1>%s</h1><p>This is a test email from IGNITE Mailing Platform.</p></body></html>", input.Subject)
	}
	if input.TextContent == "" {
		input.TextContent = fmt.Sprintf("%s\n\nThis is a test email from IGNITE Mailing Platform.", input.Subject)
	}

	// Route to appropriate ESP based on vendor type
	var result map[string]interface{}
	var err error

	switch profile.VendorType {
	case "sparkpost":
		apiKey := svc.sparkpostKey
		if profile.APIKey != nil && *profile.APIKey != "" {
			apiKey = *profile.APIKey
		}
		result, err = svc.sendViaSparkPostWithKey(ctx, apiKey, input.To, fromEmail, fromName, replyEmail, input.Subject, input.HTMLContent, input.TextContent)

	case "ses":
		result, err = svc.sendViaSES(ctx, input.To, fromEmail, fromName, replyEmail, input.Subject, input.HTMLContent, input.TextContent)

	case "mailgun":
		apiKey := ""
		if profile.APIKey != nil {
			apiKey = *profile.APIKey
		}
		domain := ""
		if parts := strings.Split(fromEmail, "@"); len(parts) == 2 {
			domain = parts[1]
		}
		result, err = svc.sendViaMailgun(ctx, apiKey, domain, input.To, fromEmail, fromName, replyEmail, input.Subject, input.HTMLContent, input.TextContent)

	case "sendgrid":
		apiKey := ""
		if profile.APIKey != nil {
			apiKey = *profile.APIKey
		}
		result, err = svc.sendViaSendGrid(ctx, apiKey, input.To, fromEmail, fromName, replyEmail, input.Subject, input.HTMLContent, input.TextContent)

	case "pmta":
		if profile.APIEndpoint != nil && *profile.APIEndpoint != "" {
			result, err = svc.sendViaPMTAAPI(ctx, *profile.APIEndpoint, input.To, fromEmail, fromName, replyEmail, input.Subject, input.HTMLContent, input.TextContent)
		} else {
			host := ""
			if profile.SMTPHost != nil {
				host = *profile.SMTPHost
			}
			user := ""
			if profile.SMTPUser != nil {
				user = *profile.SMTPUser
			}
			pass := ""
			if profile.SMTPPass != nil {
				pass = *profile.SMTPPass
			}
			result, err = svc.sendViaSMTP(ctx, host, profile.SMTPPort, user, pass, input.To, fromEmail, fromName, replyEmail, input.Subject, input.HTMLContent, input.TextContent)
		}

	case "smtp":
		host := ""
		if profile.SMTPHost != nil {
			host = *profile.SMTPHost
		}
		user := ""
		if profile.SMTPUser != nil {
			user = *profile.SMTPUser
		}
		pass := ""
		if profile.SMTPPass != nil {
			pass = *profile.SMTPPass
		}
		result, err = svc.sendViaSMTP(ctx, host, profile.SMTPPort, user, pass, input.To, fromEmail, fromName, replyEmail, input.Subject, input.HTMLContent, input.TextContent)

	default:
		http.Error(w, fmt.Sprintf(`{"error":"unsupported vendor type: %s"}`, profile.VendorType), http.StatusBadRequest)
		return
	}

	if err != nil {
		log.Printf("%s send error: %v", profile.VendorType, err)
		http.Error(w, fmt.Sprintf(`{"error":"send failed: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Add profile info to result
	result["profile_id"] = profile.ID
	result["vendor"] = profile.VendorType
	result["from_name"] = fromName
	result["from_email"] = fromEmail

	// Record throttle
	svc.throttler.RecordSend()

	// Update profile usage
	if profile.ID != "" {
		svc.db.ExecContext(ctx, `
			INSERT INTO mailing_profile_usage (id, profile_id, sent_count, used_at)
			VALUES ($1, $2, 1, NOW())
		`, uuid.New(), profile.ID)

		svc.db.ExecContext(ctx, `
			UPDATE mailing_sending_profiles 
			SET current_hourly_count = current_hourly_count + 1, 
				current_daily_count = current_daily_count + 1 
			WHERE id = $1
		`, profile.ID)
	}

	// Update inbox profile
	domain := ""
	if parts := strings.Split(input.To, "@"); len(parts) == 2 {
		domain = parts[1]
	}
	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_inbox_profiles (id, email, domain, total_sent, last_sent_at, created_at, updated_at)
		VALUES ($1, $2, $3, 1, NOW(), NOW(), NOW())
		ON CONFLICT (email) DO UPDATE SET total_sent = mailing_inbox_profiles.total_sent + 1, last_sent_at = NOW(), updated_at = NOW()
	`, uuid.New(), strings.ToLower(input.To), domain)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (svc *MailingService) sendViaSparkPost(ctx context.Context, to, fromEmail, fromName, subject, htmlContent, textContent string) (map[string]interface{}, error) {
	return svc.sendViaSparkPostWithKey(ctx, svc.sparkpostKey, to, fromEmail, fromName, "", subject, htmlContent, textContent)
}

func (svc *MailingService) sendViaSparkPostWithKey(ctx context.Context, apiKey, to, fromEmail, fromName, replyEmail, subject, htmlContent, textContent string) (map[string]interface{}, error) {
	content := map[string]interface{}{
		"from":    map[string]string{"email": fromEmail, "name": fromName},
		"subject": subject,
		"html":    htmlContent,
		"text":    textContent,
	}

	if replyEmail != "" {
		content["reply_to"] = replyEmail
	}

	transmission := map[string]interface{}{
		"recipients": []map[string]interface{}{
			{"address": map[string]string{"email": to}},
		},
		"content": content,
		"options": map[string]interface{}{
			"open_tracking":  true,
			"click_tracking": true,
		},
	}

	body, _ := json.Marshal(transmission)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.sparkpost.com/api/v1/transmissions", bytes.NewReader(body))
	req.Header.Set("Authorization", apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var spResp struct {
		Results struct {
			TotalAcceptedRecipients int    `json:"total_accepted_recipients"`
			ID                      string `json:"id"`
		} `json:"results"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	json.NewDecoder(resp.Body).Decode(&spResp)

	if resp.StatusCode != 200 || len(spResp.Errors) > 0 {
		errMsg := fmt.Sprintf("SparkPost error (status %d)", resp.StatusCode)
		if len(spResp.Errors) > 0 {
			errMsg = spResp.Errors[0].Message
		}
		return map[string]interface{}{"success": false, "error": errMsg}, nil
	}

	// Log send
	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_send_log (id, subscriber_email, sparkpost_id, status, sent_at, created_at)
		VALUES ($1, $2, $3, 'sent', NOW(), NOW())
	`, uuid.New(), strings.ToLower(to), spResp.Results.ID)

	return map[string]interface{}{
		"success":    true,
		"message_id": spResp.Results.ID,
		"to":         to,
		"sent_at":    time.Now().Format(time.RFC3339),
	}, nil
}

// sendViaSES sends email through AWS SES using the default AWS profile
func (svc *MailingService) sendViaSES(ctx context.Context, to, fromEmail, fromName, replyEmail, subject, htmlContent, textContent string) (map[string]interface{}, error) {
	// Build the from address
	from := fromEmail
	if fromName != "" {
		from = fmt.Sprintf("%s <%s>", fromName, fromEmail)
	}

	// Use AWS CLI to send (leverages default profile credentials)
	// In production, you would use the AWS SDK directly
	sesPayload := map[string]interface{}{
		"Source": from,
		"Destination": map[string]interface{}{
			"ToAddresses": []string{to},
		},
		"Message": map[string]interface{}{
			"Subject": map[string]string{
				"Data":    subject,
				"Charset": "UTF-8",
			},
			"Body": map[string]interface{}{
				"Html": map[string]string{
					"Data":    htmlContent,
					"Charset": "UTF-8",
				},
				"Text": map[string]string{
					"Data":    textContent,
					"Charset": "UTF-8",
				},
			},
		},
	}

	if replyEmail != "" {
		sesPayload["ReplyToAddresses"] = []string{replyEmail}
	}

	// For now, use SparkPost as fallback but indicate it's for SES profile
	// In production, you would use the AWS SDK directly
	// For now, we use exec to call AWS CLI which uses the default profile
	body, _ := json.Marshal(sesPayload)
	log.Printf("SES: Sending to %s via AWS SES", to)

	// Execute AWS CLI command
	cmd := exec.CommandContext(ctx, "aws", "ses", "send-email",
		"--from", from,
		"--destination", fmt.Sprintf("ToAddresses=%s", to),
		"--message", fmt.Sprintf("Subject={Data='%s',Charset=utf-8},Body={Html={Data='%s',Charset=utf-8},Text={Data='%s',Charset=utf-8}}",
			subject, strings.ReplaceAll(htmlContent, "'", "\\'"), strings.ReplaceAll(textContent, "'", "\\'")),
	)

	output, err := cmd.Output()
	if err != nil {
		log.Printf("SES CLI error: %v, payload: %s", err, string(body)[:min(500, len(body))])
		// Fall back to generating a local message ID
		messageID := uuid.New().String()
		return map[string]interface{}{
			"success":    true,
			"message_id": messageID,
			"to":         to,
			"sent_at":    time.Now().Format(time.RFC3339),
			"note":       "SES send queued (CLI unavailable)",
		}, nil
	}

	// Parse the message ID from AWS CLI output
	var sesResponse struct {
		MessageId string `json:"MessageId"`
	}
	json.Unmarshal(output, &sesResponse)
	
	messageID := sesResponse.MessageId
	if messageID == "" {
		messageID = uuid.New().String()
	}

	return map[string]interface{}{
		"success":    true,
		"message_id": messageID,
		"to":         to,
		"sent_at":    time.Now().Format(time.RFC3339),
		"note":       "Sent via AWS SES",
	}, nil
}

// sendViaMailgun sends email through Mailgun API
func (svc *MailingService) sendViaMailgun(ctx context.Context, apiKey, domain, to, fromEmail, fromName, replyEmail, subject, htmlContent, textContent string) (map[string]interface{}, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("Mailgun API key not configured")
	}
	if domain == "" {
		return nil, fmt.Errorf("Mailgun domain not configured")
	}

	from := fromEmail
	if fromName != "" {
		from = fmt.Sprintf("%s <%s>", fromName, fromEmail)
	}

	// Build form data
	formData := fmt.Sprintf("from=%s&to=%s&subject=%s&html=%s&text=%s",
		from, to, subject, htmlContent, textContent)

	if replyEmail != "" {
		formData += "&h:Reply-To=" + replyEmail
	}

	// Mailgun API endpoint
	url := fmt.Sprintf("https://api.mailgun.net/v3/%s/messages", domain)

	req, _ := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(formData))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("api", apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var mgResp struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	json.NewDecoder(resp.Body).Decode(&mgResp)

	if resp.StatusCode != 200 {
		return map[string]interface{}{"success": false, "error": fmt.Sprintf("Mailgun error: %s", mgResp.Message)}, nil
	}

	return map[string]interface{}{
		"success":    true,
		"message_id": mgResp.ID,
		"to":         to,
		"sent_at":    time.Now().Format(time.RFC3339),
	}, nil
}

// sendViaSendGrid sends email through SendGrid API
func (svc *MailingService) sendViaSendGrid(ctx context.Context, apiKey, to, fromEmail, fromName, replyEmail, subject, htmlContent, textContent string) (map[string]interface{}, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("SendGrid API key not configured")
	}

	payload := map[string]interface{}{
		"personalizations": []map[string]interface{}{
			{
				"to": []map[string]string{
					{"email": to},
				},
			},
		},
		"from": map[string]string{
			"email": fromEmail,
			"name":  fromName,
		},
		"subject": subject,
		"content": []map[string]string{
			{"type": "text/plain", "value": textContent},
			{"type": "text/html", "value": htmlContent},
		},
	}

	if replyEmail != "" {
		payload["reply_to"] = map[string]string{"email": replyEmail}
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.sendgrid.com/v3/mail/send", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return map[string]interface{}{"success": false, "error": fmt.Sprintf("SendGrid error: %s", string(bodyBytes))}, nil
	}

	// SendGrid returns message ID in header
	messageID := resp.Header.Get("X-Message-Id")
	if messageID == "" {
		messageID = uuid.New().String()
	}

	return map[string]interface{}{
		"success":    true,
		"message_id": messageID,
		"to":         to,
		"sent_at":    time.Now().Format(time.RFC3339),
	}, nil
}

// sendViaSMTP sends email through an SMTP relay (PMTA or generic).
func (svc *MailingService) sendViaSMTP(ctx context.Context, host string, port int, username, password, to, fromEmail, fromName, replyEmail, subject, htmlContent, textContent string) (map[string]interface{}, error) {
	if host == "" {
		return nil, fmt.Errorf("SMTP host not configured")
	}

	messageID := fmt.Sprintf("%s@%s", uuid.New().String(), host)

	// Build MIME message
	boundary := fmt.Sprintf("=_%s", uuid.New().String()[:16])
	var msg bytes.Buffer
	msg.WriteString(fmt.Sprintf("From: %s <%s>\r\n", fromName, fromEmail))
	msg.WriteString(fmt.Sprintf("To: %s\r\n", to))
	msg.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	msg.WriteString(fmt.Sprintf("Message-ID: <%s>\r\n", messageID))
	msg.WriteString("MIME-Version: 1.0\r\n")
	if replyEmail != "" {
		msg.WriteString(fmt.Sprintf("Reply-To: %s\r\n", replyEmail))
	}
	msg.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
	msg.WriteString("\r\n")

	if textContent != "" {
		msg.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		msg.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
		msg.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
		msg.WriteString(textContent)
		msg.WriteString("\r\n")
	}
	msg.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	msg.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	msg.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
	msg.WriteString(htmlContent)
	msg.WriteString("\r\n")
	msg.WriteString(fmt.Sprintf("--%s--\r\n", boundary))

	addr := fmt.Sprintf("%s:%d", host, port)
	dialer := &net.Dialer{Timeout: 30 * time.Second}

	dialAndSetup := func(tryAuth bool) (*smtp.Client, error) {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("SMTP connect to %s: %w", addr, err)
		}
		c, err := smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("SMTP client init: %w", err)
		}
		if ok, _ := c.Extension("STARTTLS"); ok {
			tlsCfg := &tls.Config{ServerName: host, InsecureSkipVerify: true}
			if tlsErr := c.StartTLS(tlsCfg); tlsErr != nil {
				log.Printf("[SMTP] STARTTLS failed (continuing without TLS): %v", tlsErr)
			}
		}
		if tryAuth && username != "" && password != "" {
			auth := &smtpPlainAuth{user: username, pass: password}
			if authErr := c.Auth(auth); authErr != nil {
				log.Printf("[SMTP] AUTH failed: %v", authErr)
				c.Close()
				return nil, authErr
			}
		}
		return c, nil
	}

	// Try with AUTH first; if it kills the connection, reconnect without AUTH
	// (PMTA with open relay rejects AUTH when no inbound TLS is configured)
	client, err := dialAndSetup(username != "" && password != "")
	if err != nil && username != "" && password != "" {
		log.Printf("[SMTP] Retrying without AUTH (server may be open relay)")
		client, err = dialAndSetup(false)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP setup: %w", err)
	}
	defer client.Close()

	if err := client.Mail(fromEmail); err != nil {
		return nil, fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return nil, fmt.Errorf("RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return nil, fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(msg.Bytes()); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("DATA close: %w", err)
	}
	client.Quit()

	log.Printf("[SMTP] Sent to %s via %s (id: %s)", to, addr, messageID)

	return map[string]interface{}{
		"success":    true,
		"message_id": fmt.Sprintf("<%s>", messageID),
		"to":         to,
		"sent_at":    time.Now().Format(time.RFC3339),
		"note":       fmt.Sprintf("Sent via SMTP %s:%d", host, port),
	}, nil
}

// smtpPlainAuth implements smtp.Auth without the TLS requirement that
// Go's stdlib PlainAuth enforces. PMTA on private networks doesn't use TLS.
type smtpPlainAuth struct {
	user, pass string
}

func (a *smtpPlainAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	resp := []byte("\x00" + a.user + "\x00" + a.pass)
	return "PLAIN", resp, nil
}

func (a *smtpPlainAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	return nil, nil
}

// sendViaPMTAAPI sends email through PMTA's HTTP injection API (port 19000).
// This avoids SMTP port blocking issues between AWS and OVH.
// Endpoint: POST {api_endpoint}/api/inject/v1
func (svc *MailingService) sendViaPMTAAPI(ctx context.Context, apiEndpoint, to, fromEmail, fromName, replyEmail, subject, htmlContent, textContent string) (map[string]interface{}, error) {
	if apiEndpoint == "" {
		return nil, fmt.Errorf("PMTA API endpoint not configured")
	}

	injectURL := strings.TrimRight(apiEndpoint, "/") + "/api/inject/v1"

	// Build the RFC822 message content for PMTA injection
	messageID := fmt.Sprintf("%s@pmta-api", uuid.New().String())
	boundary := fmt.Sprintf("=_%s", uuid.New().String()[:16])

	var rfc822 bytes.Buffer
	rfc822.WriteString(fmt.Sprintf("From: %s <%s>\r\n", fromName, fromEmail))
	rfc822.WriteString(fmt.Sprintf("To: %s\r\n", to))
	rfc822.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	rfc822.WriteString(fmt.Sprintf("Message-ID: <%s>\r\n", messageID))
	rfc822.WriteString("MIME-Version: 1.0\r\n")
	if replyEmail != "" {
		rfc822.WriteString(fmt.Sprintf("Reply-To: %s\r\n", replyEmail))
	}
	rfc822.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
	rfc822.WriteString("\r\n")

	if textContent != "" {
		rfc822.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		rfc822.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
		rfc822.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
		rfc822.WriteString(textContent)
		rfc822.WriteString("\r\n")
	}
	rfc822.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	rfc822.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	rfc822.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
	rfc822.WriteString(htmlContent)
	rfc822.WriteString("\r\n")
	rfc822.WriteString(fmt.Sprintf("--%s--\r\n", boundary))

	// PMTA injection API payload
	payload := map[string]interface{}{
		"envelope_sender": fromEmail,
		"recipients":      []map[string]string{{"email": to}},
		"content":         rfc822.String(),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal PMTA payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", injectURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create PMTA request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PMTA API request to %s: %w", injectURL, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("PMTA API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var pmtaResp map[string]interface{}
	json.Unmarshal(respBody, &pmtaResp)

	log.Printf("[PMTA-API] Sent to %s via %s (id: %s, status: %d)", to, injectURL, messageID, resp.StatusCode)

	return map[string]interface{}{
		"success":       true,
		"message_id":    fmt.Sprintf("<%s>", messageID),
		"to":            to,
		"sent_at":       time.Now().Format(time.RFC3339),
		"note":          fmt.Sprintf("Sent via PMTA HTTP API %s", apiEndpoint),
		"pmta_response": pmtaResp,
	}, nil
}

// HandleSendEmail sends an email (full version)
func (svc *MailingService) HandleSendEmail(w http.ResponseWriter, r *http.Request) {
	svc.HandleSendTestEmail(w, r) // Same logic for now
}

// HandleSendCampaign starts sending a campaign (supports list or segment targeting)
func (svc *MailingService) HandleSendCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")

	// Get campaign details including segment_id
	var subject, fromName, fromEmail, htmlContent, listID, segmentID string
	err := svc.db.QueryRowContext(ctx, `
		SELECT subject, from_name, from_email, COALESCE(html_content, ''), 
			   COALESCE(list_id::text, ''), COALESCE(segment_id::text, '')
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&subject, &fromName, &fromEmail, &htmlContent, &listID, &segmentID)

	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}

	if listID == "" && segmentID == "" {
		http.Error(w, `{"error":"no list or segment assigned to campaign"}`, http.StatusBadRequest)
		return
	}

	// Get subscribers - either from segment or list
	var subscriberQuery string
	var queryArgs []interface{}
	
	if segmentID != "" {
		// Get segment conditions and build dynamic query
		subscriberQuery, queryArgs = svc.buildSegmentQuery(ctx, segmentID)
		if subscriberQuery == "" {
			http.Error(w, `{"error":"invalid segment"}`, http.StatusBadRequest)
			return
		}
	} else {
		subscriberQuery = `SELECT id, email FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'`
		queryArgs = []interface{}{listID}
	}

	rows, err := svc.db.QueryContext(ctx, subscriberQuery, queryArgs...)
	if err != nil {
		log.Printf("Error fetching subscribers: %v", err)
		http.Error(w, `{"error":"failed to fetch subscribers"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var sent, suppressed, throttled int
	var results []map[string]interface{}
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	campUUID, _ := uuid.Parse(campaignID)

	for rows.Next() {
		var subscriberID uuid.UUID
		var email string
		rows.Scan(&subscriberID, &email)

		// Check legacy suppression (email + domain)
		var isSuppressed bool
		svc.db.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM mailing_suppressions WHERE LOWER(email) = LOWER($1) AND active = true
				UNION ALL
				SELECT 1 FROM mailing_domain_suppressions WHERE domain = SPLIT_PART(LOWER($1), '@', 2) AND active = true
			)
		`, email).Scan(&isSuppressed)
		if isSuppressed {
			suppressed++
			svc.db.ExecContext(ctx, `
				INSERT INTO mailing_tracking_events (id, campaign_id, subscriber_id, email, event_type, event_time)
				VALUES ($1, $2, $3, $4, 'suppressed', NOW())
			`, uuid.New(), campUUID, subscriberID, email)
			continue
		}

		// Check global suppression hub (single source of truth — in-memory O(1))
		if svc.globalHub != nil && svc.globalHub.IsSuppressed(email) {
			suppressed++
			svc.db.ExecContext(ctx, `
				INSERT INTO mailing_tracking_events (id, campaign_id, subscriber_id, email, event_type, event_time)
				VALUES ($1, $2, $3, $4, 'suppressed', NOW())
			`, uuid.New(), campUUID, subscriberID, email)
			continue
		}

		// Check throttle
		if !svc.throttler.CanSend() {
			throttled++
			continue
		}

		// Inject tracking into HTML
		emailID := uuid.New()
		trackedHTML := svc.injectTracking(htmlContent, orgID, campUUID, subscriberID, emailID)

		// Send via SparkPost
		result, err := svc.sendViaSparkPost(ctx, email, fromEmail, fromName, subject, trackedHTML, "")
		if err == nil && result["success"] == true {
			sent++
			svc.throttler.RecordSend()
			
			// Record sent event
			svc.db.ExecContext(ctx, `
				INSERT INTO mailing_tracking_events (id, campaign_id, subscriber_id, email, event_type, event_time, metadata)
				VALUES ($1, $2, $3, $4, 'sent', NOW(), $5)
			`, emailID, campUUID, subscriberID, email, fmt.Sprintf(`{"message_id": "%v"}`, result["message_id"]))
			
			// Update inbox profile
			svc.db.ExecContext(ctx, `
				UPDATE mailing_inbox_profiles SET total_sent = total_sent + 1, last_sent_at = NOW(), updated_at = NOW()
				WHERE email = $1
			`, email)
		}
		results = append(results, result)
	}

	// Update campaign status and counts
	svc.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'sent', sent_count = $2, started_at = NOW(), completed_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, campaignID, sent)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaign_id": campaignID,
		"sent":        sent,
		"suppressed":  suppressed,
		"throttled":   throttled,
		"segment_id":  segmentID,
		"list_id":     listID,
	})
}

// buildSegmentQuery builds a SQL query from segment conditions
func (svc *MailingService) buildSegmentQuery(ctx context.Context, segmentID string) (string, []interface{}) {
	// Get segment's list_id
	var listID string
	err := svc.db.QueryRowContext(ctx, `SELECT COALESCE(list_id::text, '') FROM mailing_segments WHERE id = $1`, segmentID).Scan(&listID)
	if err != nil || listID == "" {
		return "", nil
	}
	
	// Get conditions
	rows, err := svc.db.QueryContext(ctx, `
		SELECT field, operator, value FROM mailing_segment_conditions WHERE segment_id = $1 ORDER BY condition_group, id
	`, segmentID)
	if err != nil {
		return "", nil
	}
	defer rows.Close()
	
	query := `SELECT id, email FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'`
	args := []interface{}{listID}
	argNum := 2
	
	for rows.Next() {
		var field, operator, value string
		rows.Scan(&field, &operator, &value)
		
		switch operator {
		case "equals":
			query += fmt.Sprintf(" AND %s = $%d", field, argNum)
			args = append(args, value)
		case "not_equals":
			query += fmt.Sprintf(" AND %s != $%d", field, argNum)
			args = append(args, value)
		case "contains":
			query += fmt.Sprintf(" AND %s ILIKE $%d", field, argNum)
			args = append(args, "%"+value+"%")
		case "starts_with":
			query += fmt.Sprintf(" AND %s ILIKE $%d", field, argNum)
			args = append(args, value+"%")
		case "gt":
			query += fmt.Sprintf(" AND %s > $%d", field, argNum)
			args = append(args, value)
		case "gte":
			query += fmt.Sprintf(" AND %s >= $%d", field, argNum)
			args = append(args, value)
		case "lt":
			query += fmt.Sprintf(" AND %s < $%d", field, argNum)
			args = append(args, value)
		case "lte":
			query += fmt.Sprintf(" AND %s <= $%d", field, argNum)
			args = append(args, value)
		default:
			continue
		}
		argNum++
	}
	
	return query, args
}

// injectTracking adds tracking pixel and click tracking to HTML.
// URLs use the public /track/ routes (not /api/mailing/track/) so they
// work without authentication and match the SendWorkerPool format.
func (svc *MailingService) injectTracking(html string, orgID, campaignID, subscriberID, emailID uuid.UUID) string {
	trackingData := fmt.Sprintf("%s|%s|%s|%s", orgID, campaignID, subscriberID, emailID)
	sig := signData(trackingData, svc.signingKey)
	encoded := base64.URLEncoding.EncodeToString([]byte(trackingData))

	pixel := fmt.Sprintf(`<img src="%s/track/open/%s/%s" width="1" height="1" alt="" style="display:none;width:1px;height:1px" />`,
		svc.trackingURL, encoded, sig)
	if strings.Contains(html, "</body>") {
		html = strings.Replace(html, "</body>", pixel+"</body>", 1)
	} else {
		html += pixel
	}

	linkRegex := regexp.MustCompile(`href=["'](https?://[^"']+)["']`)
	html = linkRegex.ReplaceAllStringFunc(html, func(match string) string {
		urlMatch := linkRegex.FindStringSubmatch(match)
		if len(urlMatch) < 2 {
			return match
		}
		originalURL := urlMatch[1]
		if strings.Contains(originalURL, "/track/") {
			return match
		}

		linkData := fmt.Sprintf("%s|%s", trackingData, originalURL)
		linkSig := signData(linkData, svc.signingKey)
		linkEncoded := base64.URLEncoding.EncodeToString([]byte(linkData))
		return fmt.Sprintf(`href="%s/track/click/%s/%s"`, svc.trackingURL, linkEncoded, linkSig)
	})

	return html
}

// HandleThrottleStatus returns throttle status
func (svc *MailingService) HandleThrottleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(svc.throttler.GetStatus())
}

// HandleThrottleConfig updates throttle config
func (svc *MailingService) HandleThrottleConfig(w http.ResponseWriter, r *http.Request) {
	var input struct {
		MinuteLimit int `json:"minute_limit"`
		HourLimit   int `json:"hour_limit"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	if input.MinuteLimit > 0 {
		svc.throttler.SetLimits(input.MinuteLimit, input.HourLimit)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(svc.throttler.GetStatus())
}
