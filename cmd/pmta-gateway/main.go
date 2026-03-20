package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Metrics counters (atomic for lock-free reads in /metrics).
var (
	totalInjected   int64
	totalFailed     int64
	totalRejected   int64 // pre-validation rejections
	totalBytesSent  int64
)

var cache *vmtaCache
var startTime time.Time

// injectRequest matches the JSON schema used by the Python bridge.
type injectRequest struct {
	EnvelopeSender string              `json:"envelope_sender"`
	Recipients     []map[string]string `json:"recipients"`
	Content        string              `json:"content"`
	VMTA           string              `json:"vmta"`
	JobID          string              `json:"job_id"`
}

type injectResponse struct {
	Status    string `json:"status"`
	MessageID string `json:"message_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

func main() {
	startTime = time.Now()

	listenAddr := envOr("LISTEN_ADDR", ":19099")
	smtpAddr := envOr("SMTP_ADDR", "127.0.0.1:25")
	vmtaRefresh := 60 * time.Second

	cache = newVMTACache()
	cache.StartRefreshLoop(vmtaRefresh)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/inject/v1", handleInject(smtpAddr))
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/metrics", handleMetrics)

	log.Printf("[gateway] starting on %s, SMTP relay %s", listenAddr, smtpAddr)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("[gateway] server error: %v", err)
	}
}

func handleInject(smtpAddr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			respondErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		var req injectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			atomic.AddInt64(&totalRejected, 1)
			respondErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		// Validation: content must be valid UTF-8
		if !utf8.ValidString(req.Content) {
			atomic.AddInt64(&totalRejected, 1)
			respondErr(w, http.StatusBadRequest, "content is not valid UTF-8")
			return
		}

		// Validation: VMTA must be non-empty
		if req.VMTA == "" {
			atomic.AddInt64(&totalRejected, 1)
			respondErr(w, http.StatusBadRequest, "vmta field is required")
			return
		}

		// Validation: VMTA must exist in local PMTA config
		if !cache.Has(req.VMTA) {
			atomic.AddInt64(&totalRejected, 1)
			respondErr(w, http.StatusBadRequest, fmt.Sprintf("vmta '%s' does not exist on this server", req.VMTA))
			return
		}

		// Validation: envelope sender required
		if req.EnvelopeSender == "" {
			atomic.AddInt64(&totalRejected, 1)
			respondErr(w, http.StatusBadRequest, "envelope_sender is required")
			return
		}

		// Validation: at least one recipient
		if len(req.Recipients) == 0 {
			atomic.AddInt64(&totalRejected, 1)
			respondErr(w, http.StatusBadRequest, "at least one recipient is required")
			return
		}

		recipientEmails := make([]string, 0, len(req.Recipients))
		for _, rcpt := range req.Recipients {
			email := rcpt["email"]
			if email == "" {
				atomic.AddInt64(&totalRejected, 1)
				respondErr(w, http.StatusBadRequest, "recipient email is empty")
				return
			}
			recipientEmails = append(recipientEmails, email)
		}

		// Build the RFC822 message with X-Virtual-MTA header prepended
		var msg strings.Builder
		msg.WriteString(fmt.Sprintf("X-Virtual-MTA: %s\r\n", req.VMTA))
		if req.JobID != "" {
			msg.WriteString(fmt.Sprintf("X-Job: %s\r\n", req.JobID))
		}
		msg.WriteString(req.Content)
		msgBytes := []byte(msg.String())

		// Inject via local SMTP
		start := time.Now()
		err := smtpSend(smtpAddr, req.EnvelopeSender, recipientEmails, msgBytes)
		elapsed := time.Since(start)

		recipientDomain := ""
		if len(recipientEmails) > 0 {
			if idx := strings.LastIndex(recipientEmails[0], "@"); idx >= 0 {
				recipientDomain = recipientEmails[0][idx+1:]
			}
		}

		if err != nil {
			atomic.AddInt64(&totalFailed, 1)
			log.Printf("[gateway] FAIL vmta=%s from=%s to=%s domain=%s err=%q elapsed=%s",
				req.VMTA, req.EnvelopeSender, recipientEmails[0], recipientDomain, err.Error(), elapsed)
			respondErr(w, http.StatusBadGateway, "SMTP relay failed: "+err.Error())
			return
		}

		atomic.AddInt64(&totalInjected, 1)
		atomic.AddInt64(&totalBytesSent, int64(len(msgBytes)))
		log.Printf("[gateway] OK vmta=%s from=%s to=%s domain=%s bytes=%d elapsed=%s",
			req.VMTA, req.EnvelopeSender, recipientEmails[0], recipientDomain, len(msgBytes), elapsed)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(injectResponse{
			Status:    "ok",
			MessageID: fmt.Sprintf("%d", time.Now().UnixNano()),
		})
	}
}

// smtpSend connects to the local SMTP server and sends the message.
// Uses raw net.Conn + SMTP protocol to avoid Go's net/smtp requiring
// a valid hostname (the local PMTA doesn't need EHLO validation).
func smtpSend(addr, from string, to []string, msg []byte) error {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	buf := make([]byte, 512)

	// Read greeting
	if _, err := conn.Read(buf); err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}

	// EHLO
	fmt.Fprintf(conn, "EHLO localhost\r\n")
	if _, err := conn.Read(buf); err != nil {
		return fmt.Errorf("read EHLO response: %w", err)
	}

	// MAIL FROM
	fmt.Fprintf(conn, "MAIL FROM:<%s>\r\n", from)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read MAIL FROM response: %w", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "250") {
		return fmt.Errorf("MAIL FROM rejected: %s", strings.TrimSpace(string(buf[:n])))
	}

	// RCPT TO
	for _, rcpt := range to {
		fmt.Fprintf(conn, "RCPT TO:<%s>\r\n", rcpt)
		n, err = conn.Read(buf)
		if err != nil {
			return fmt.Errorf("read RCPT TO response: %w", err)
		}
		if !strings.HasPrefix(string(buf[:n]), "250") {
			return fmt.Errorf("RCPT TO <%s> rejected: %s", rcpt, strings.TrimSpace(string(buf[:n])))
		}
	}

	// DATA
	fmt.Fprintf(conn, "DATA\r\n")
	n, err = conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read DATA response: %w", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "354") {
		return fmt.Errorf("DATA rejected: %s", strings.TrimSpace(string(buf[:n])))
	}

	// Send message body + terminator
	conn.Write(msg)
	if !strings.HasSuffix(string(msg), "\r\n") {
		conn.Write([]byte("\r\n"))
	}
	conn.Write([]byte(".\r\n"))
	n, err = conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read DATA end response: %w", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "250") {
		return fmt.Errorf("message rejected: %s", strings.TrimSpace(string(buf[:n])))
	}

	// QUIT
	fmt.Fprintf(conn, "QUIT\r\n")
	return nil
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "ok",
		"vmta_count":     cache.Count(),
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	injected := atomic.LoadInt64(&totalInjected)
	failed := atomic.LoadInt64(&totalFailed)
	rejected := atomic.LoadInt64(&totalRejected)
	bytesSent := atomic.LoadInt64(&totalBytesSent)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP gateway_injections_total Total successful injections\n")
	fmt.Fprintf(w, "# TYPE gateway_injections_total counter\n")
	fmt.Fprintf(w, "gateway_injections_total %d\n\n", injected)

	fmt.Fprintf(w, "# HELP gateway_failures_total Total SMTP relay failures\n")
	fmt.Fprintf(w, "# TYPE gateway_failures_total counter\n")
	fmt.Fprintf(w, "gateway_failures_total %d\n\n", failed)

	fmt.Fprintf(w, "# HELP gateway_rejections_total Total pre-validation rejections\n")
	fmt.Fprintf(w, "# TYPE gateway_rejections_total counter\n")
	fmt.Fprintf(w, "gateway_rejections_total %d\n\n", rejected)

	fmt.Fprintf(w, "# HELP gateway_bytes_sent_total Total bytes sent to SMTP\n")
	fmt.Fprintf(w, "# TYPE gateway_bytes_sent_total counter\n")
	fmt.Fprintf(w, "gateway_bytes_sent_total %d\n\n", bytesSent)

	fmt.Fprintf(w, "# HELP gateway_vmta_count Number of known VMTAs\n")
	fmt.Fprintf(w, "# TYPE gateway_vmta_count gauge\n")
	fmt.Fprintf(w, "gateway_vmta_count %d\n\n", cache.Count())

	fmt.Fprintf(w, "# HELP gateway_uptime_seconds Seconds since start\n")
	fmt.Fprintf(w, "# TYPE gateway_uptime_seconds gauge\n")
	fmt.Fprintf(w, "gateway_uptime_seconds %d\n", int(time.Since(startTime).Seconds()))
}

func respondErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(injectResponse{Status: "error", Error: msg})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
