package worker

// Journey send-retry policy.
//
// WHY: a node error used to do exactly two things — write an 'error' row to the
// execution log and return. The claim lease then pushed next_execute_at forward
// by a couple of minutes, the enrollment was reclaimed, and it failed again.
// Forever. Measured on prod 2026-08-25, offer 420 node email-3:
//
//	enroll-clk-7d40ce26  9,340 attempts  2026-08-12 -> 2026-08-25 (still firing)
//	enroll-clk-35b753d2  8,947 attempts  2026-08-13 -> 2026-08-25
//	enroll-clk-57252f12  8,623 attempts  2026-08-13 -> 2026-08-25
//
// all with "all IPs exhausted warmup limits". THREE mailboxes produced 26,908
// error rows, which the funnel screen rendered as "26,904 send errors".
//
// A max-attempt cap alone turns an infinite loop into a finite one; it does not
// make the retry system correct. This file adds the four things that do:
// error CLASSIFICATION (a 422 must never be retried at all), EXPONENTIAL
// BACKOFF (so a capacity error stops hammering), a MAX ELAPSED window (so a
// slow drip of retries still terminates), and a reason-coded TERMINAL EXIT so
// the enrollment leaves the lane instead of living in the log.

import (
	"context"
	"database/sql"
	"log"
	"math"
	"os"
	"strings"
	"time"
)

// journeyRetryEjectDisabled is a SEPARATE kill switch from the backoff.
//
// The two halves of this policy carry different risk. Backoff stops the hot
// loop and is purely subtractive — worst case a send is slower. Ejection MUTATES
// a live enrollment: it ends someone's sequence, and re-enrolling them needs the
// upload path. If classification ever proves too eager in production, this
// disables the ejection alone and leaves the loop fix in place.
//
// JOURNEY_RETRY_EJECT_DISABLED=1
func journeyRetryEjectDisabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("JOURNEY_RETRY_EJECT_DISABLED")))
	return v == "1" || v == "true" || v == "yes"
}

const (
	// journeyRetryMaxAttempts bounds attempts at ONE node. With the backoff
	// curve below this spans roughly 24h before the elapsed cap bites.
	journeyRetryMaxAttempts = 12

	// journeyRetryMaxElapsed bounds wall-clock retrying at one node regardless
	// of attempt count. Whichever cap trips first ends the enrollment.
	journeyRetryMaxElapsed = 24 * time.Hour

	// journeyRetryBaseDelay / journeyRetryMaxDelay shape the backoff.
	journeyRetryBaseDelay = 5 * time.Minute
	journeyRetryMaxDelay  = 6 * time.Hour
)

// journeyRetryClass is what we decided to do about an error.
type journeyRetryClass string

const (
	journeyRetryTransient journeyRetryClass = "transient"
	journeyRetryTerminal  journeyRetryClass = "terminal"
)

// journeyTerminalMarkers are errors that CANNOT succeed on retry. Retrying a
// malformed request or an unconfigured profile just burns the lane's execution
// budget and fills the log.
var journeyTerminalMarkers = []string{
	"http 400", "http 404", "http 409", "http 422",
	"no sending ips configured",
	"invalid email",
	"suppressed",
	"not approved",
	"unknown sending profile",
}

// classifyJourneySendError decides whether an error may be retried.
//
// Default is TRANSIENT: an unrecognized error is more likely a blip than a
// permanent condition, and the attempt/elapsed caps bound it either way. Only
// errors we can name as unrecoverable are terminal on the first try.
func classifyJourneySendError(err error) journeyRetryClass {
	if err == nil {
		return journeyRetryTransient
	}
	msg := strings.ToLower(err.Error())
	for _, m := range journeyTerminalMarkers {
		if strings.Contains(msg, m) {
			return journeyRetryTerminal
		}
	}
	return journeyRetryTransient
}

// journeyRetryBackoff is exponential with a ceiling: 5m, 10m, 20m, 40m, 80m,
// 160m, 320m, then flat at 6h.
func journeyRetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 20 {
		attempt = 20
	}
	d := time.Duration(float64(journeyRetryBaseDelay) * math.Pow(2, float64(attempt-1)))
	if d > journeyRetryMaxDelay || d <= 0 {
		return journeyRetryMaxDelay
	}
	return d
}

// journeyRetryDecision is the outcome of recording one failure.
type journeyRetryDecision struct {
	Class     journeyRetryClass
	Attempts  int
	Terminal  bool
	Reason    string
	NextDelay time.Duration
}

// recordJourneySendFailure books one failure against (enrollment, node) and
// decides what happens next. It is the ONLY place that decides to eject.
//
// Attempt state lives on the enrollment row (retry_node_id / retry_attempts /
// retry_first_at) rather than being counted from the execution log: counting
// 26,908 log rows per tick to decide whether to write the 26,909th is exactly
// the cost this is meant to remove.
//
// Moving to a different node RESETS the counters — a lane that failed twice at
// touch 2 and then succeeded must not carry that history into touch 3.
// journeyRetryCtx detaches retry bookkeeping from the FAILED SEND's context.
//
// WHY (verified in production 2026-08-25, on the very deploy that added this
// policy): every write in this file inherited the caller's ctx — which, at the
// moment a send fails, is frequently ALREADY EXPIRED, because an expired
// context is one of the things that makes the send fail. So the backoff and the
// eject could never land, and the policy was inert on exactly the enrollments
// it exists for:
//
//	[JourneyRetry] bookkeeping failed (enrollment=enroll-clk-7d40ce26 node=email-3): context deadline exceeded
//	[JourneyRetry] defer failed (enrollment=enroll-clk-7d40ce26): context deadline exceeded
//
// enroll-clk-7d40ce26 is one of the three mailboxes that had been retrying every
// ~2 minutes for 13 days. A gate that no-ops is worse than no gate.
//
// WithoutCancel keeps the context VALUES (tracing, org scope) and drops only the
// deadline and cancellation; the 5s budget bounds the detached write so a wedged
// database cannot stall the executor goroutine that is trying to give up.
func journeyRetryCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func recordJourneySendFailure(ctx context.Context, db *sql.DB, enrollmentID, nodeID string, sendErr error) journeyRetryDecision {
	ctx, cancel := journeyRetryCtx(ctx)
	defer cancel()

	class := classifyJourneySendError(sendErr)
	d := journeyRetryDecision{Class: class}

	var attempts int
	var firstAt sql.NullTime
	err := db.QueryRowContext(ctx, `
		UPDATE mailing_journey_enrollments
		   SET retry_node_id  = $2,
		       retry_attempts = CASE WHEN retry_node_id IS DISTINCT FROM $2 THEN 1 ELSE COALESCE(retry_attempts,0) + 1 END,
		       retry_first_at = CASE WHEN retry_node_id IS DISTINCT FROM $2 THEN NOW() ELSE COALESCE(retry_first_at, NOW()) END,
		       updated_at     = NOW()
		 WHERE id = $1
		RETURNING retry_attempts, retry_first_at
	`, enrollmentID, nodeID).Scan(&attempts, &firstAt)
	if err != nil {
		// Retry bookkeeping must never be the reason a send is lost. Treat an
		// accounting failure as "transient, try again later" and log it.
		if !isMissingRetryColumns(err) {
			log.Printf("[JourneyRetry] bookkeeping failed (enrollment=%s node=%s): %v", enrollmentID, nodeID, err)
		}
		d.Attempts = 1
		d.NextDelay = journeyRetryBackoff(1)
		return d
	}

	d.Attempts = attempts
	elapsed := time.Duration(0)
	if firstAt.Valid {
		elapsed = time.Since(firstAt.Time)
	}

	// With ejection disabled the enrollment still backs off — it just never
	// leaves the lane. The cap is honoured as a flat max-delay hold so a
	// permanently failing send cannot go back to hammering.
	if journeyRetryEjectDisabled() {
		d.NextDelay = journeyRetryBackoff(attempts)
		return d
	}

	switch {
	case class == journeyRetryTerminal:
		d.Terminal, d.Reason = true, "send_failed_terminal"
	case attempts >= journeyRetryMaxAttempts:
		d.Terminal, d.Reason = true, "send_failed_max_attempts"
	case elapsed >= journeyRetryMaxElapsed:
		d.Terminal, d.Reason = true, "send_failed_max_elapsed"
	default:
		d.NextDelay = journeyRetryBackoff(attempts)
	}
	return d
}

// clearJourneyRetry resets attempt state after a node succeeds. Guarded so it
// only writes when there is something to clear.
func clearJourneyRetry(ctx context.Context, db *sql.DB, enrollmentID string) {
	if db == nil {
		return
	}
	ctx, cancel := journeyRetryCtx(ctx)
	defer cancel()

	if _, err := db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments
		   SET retry_node_id = NULL, retry_attempts = 0, retry_first_at = NULL
		 WHERE id = $1 AND (retry_node_id IS NOT NULL OR COALESCE(retry_attempts,0) <> 0)
	`, enrollmentID); err != nil && !isMissingRetryColumns(err) {
		log.Printf("[JourneyRetry] clear failed (enrollment=%s): %v", enrollmentID, err)
	}
}

// ejectJourneyEnrollment ends an enrollment that cannot be sent.
//
// The exit reason is REASON-CODED and prefixed so the snapshot's exit
// classifier files it as behavioral (a real lane outcome), never as an
// administrative operator purge.
func ejectJourneyEnrollment(ctx context.Context, db *sql.DB, enrollmentID, nodeID, reason string, sendErr error) {
	ctx, cancel := journeyRetryCtx(ctx)
	defer cancel()

	msg := reason
	if sendErr != nil {
		m := sendErr.Error()
		if len(m) > 300 {
			m = m[:300]
		}
		msg = reason + " @" + nodeID + ": " + m
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments
		   SET status = 'exited', exited_at = NOW(), exit_reason = $2,
		       next_execute_at = NULL, retry_node_id = NULL, retry_attempts = 0,
		       retry_first_at = NULL, updated_at = NOW()
		 WHERE id = $1 AND status = 'active'
	`, enrollmentID, msg); err != nil {
		log.Printf("[JourneyRetry] eject failed (enrollment=%s): %v", enrollmentID, err)
		return
	}
	log.Printf("[JourneyRetry] EJECTED enrollment=%s node=%s reason=%s", enrollmentID, nodeID, reason)
}

// deferJourneyEnrollment pushes the next attempt out by the backoff delay.
// Without this the claim lease alone decides the retry cadence, which is how a
// failing send became a two-minute hot loop for thirteen days.
func deferJourneyEnrollment(ctx context.Context, db *sql.DB, enrollmentID string, delay time.Duration) {
	ctx, cancel := journeyRetryCtx(ctx)
	defer cancel()

	if delay <= 0 {
		delay = journeyRetryBaseDelay
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments
		   SET next_execute_at = NOW() + make_interval(secs => $2), updated_at = NOW()
		 WHERE id = $1 AND status = 'active'
	`, enrollmentID, int(delay.Seconds())); err != nil {
		log.Printf("[JourneyRetry] defer failed (enrollment=%s): %v", enrollmentID, err)
	}
}

// isMissingRetryColumns lets the executor run unchanged in the window between
// this binary and its migration. Schema-coupling a send path to reporting
// metadata is what took the funnel screen down on 2026-08-02.
func isMissingRetryColumns(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "does not exist") &&
		(strings.Contains(s, "retry_node_id") || strings.Contains(s, "retry_attempts") || strings.Contains(s, "retry_first_at"))
}
