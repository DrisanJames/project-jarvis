package smtputil

import (
	"regexp"
	"strings"
)

// BounceType classifies an SMTP failure as hard (permanent) or soft (transient).
type BounceType string

const (
	BounceHard BounceType = "hard"
	BounceSoft BounceType = "soft"
)

// BounceClass is the three-way classification of a delivery failure.
//
// The distinction matters operationally: a hard bounce is a list-hygiene signal
// (the address is permanently bad) and the recipient SHOULD be suppressed. A
// reputation/system block is a SENDER-side signal (the receiving system is
// refusing our mail for policy/reputation/throughput reasons) — the recipient
// is perfectly valid and must NOT be suppressed; the remedy is warmup / volume
// pacing / reputation work. Soft bounces are transient and clear on retry.
type BounceClass int

const (
	ClassHard  BounceClass = iota // invalid address, dead domain, disabled mailbox
	ClassBlock                    // reputation/system/policy block — recoverable
	ClassSoft                     // transient (quota full, rate limit, timeouts)
)

// dsnRe extracts a leading RFC 3463 enhanced status code (e.g. "5.3.2") from a
// diagnostic string. PMTA accounting writes bounce_reason as "X.Y.Z (text)".
var dsnRe = regexp.MustCompile(`\b([245])\.\d{1,3}\.\d{1,3}\b`)

// replyCodeRe matches a permanent (5xx) SMTP reply code as a standalone token,
// avoiding substring false positives (e.g. "550" inside a byte count).
var replyCodeRe = regexp.MustCompile(`(^|[^0-9])(55[0-5])([^0-9]|$)`)

// ExtractDSN returns the first RFC 3463 enhanced status code found in s, or "".
func ExtractDSN(s string) string {
	return dsnRe.FindString(s)
}

// IsReputationOrSystemBlock reports whether a DSN enhanced status code denotes a
// system / network / protocol / security-policy condition rather than a bad
// recipient address. These are the "subject" sub-classes of RFC 3463:
//
//	x.3.x  Mail system status   (e.g. 5.3.2 "system not accepting network messages")
//	x.4.x  Network/routing status
//	x.5.x  Mail delivery protocol status
//	x.7.x  Security/policy status (e.g. 5.7.1 "blocked"/"rejected due to policy")
//
// A permanent (5.x) code in any of these families is a reputation/system block,
// NOT list hygiene. Charter/Spectrum returning "5.3.2 system not accepting
// network messages" to a warming domain is the canonical example — PMTA labels
// it bad-mailbox, but the address is valid and must not be suppressed.
func IsReputationOrSystemBlock(dsn string) bool {
	dsn = strings.TrimSpace(dsn)
	if !strings.HasPrefix(dsn, "5.") {
		return false
	}
	switch {
	case strings.HasPrefix(dsn, "5.3."),
		strings.HasPrefix(dsn, "5.4."),
		strings.HasPrefix(dsn, "5.5."),
		strings.HasPrefix(dsn, "5.7."):
		return true
	}
	return false
}

// isGenuineHardCategory reports whether a PMTA bounce category denotes a
// list-hygiene failure (after DSN-based blocks have been excluded by the
// caller). Connection-level categories (no-answer-from-host, bad-connection)
// and reputation categories are intentionally absent.
func isGenuineHardCategory(cat string) bool {
	switch cat {
	case "hard", "bad-mailbox", "bad-domain", "inactive-mailbox":
		return true
	}
	return false
}

// ClassifyBounce performs the authoritative three-way classification using the
// DSN enhanced status code as the source of truth and falling back to PMTA's
// coarse bounce category. The DSN wins because PMTA mislabels ISP system/policy
// rejections (e.g. Charter/Spectrum "5.3.2") as bad-mailbox.
func ClassifyBounce(dsnStatus, bounceCat, diag string) BounceClass {
	dsn := strings.TrimSpace(dsnStatus)
	if dsn == "" {
		dsn = ExtractDSN(diag)
	}

	if strings.HasPrefix(dsn, "4.") {
		return ClassSoft
	}
	if IsReputationOrSystemBlock(dsn) {
		return ClassBlock
	}
	if strings.HasPrefix(dsn, "5.1.") { // bad destination address/mailbox/system
		return ClassHard
	}
	if dsn == "5.2.1" { // mailbox disabled — permanent
		return ClassHard
	}
	if strings.HasPrefix(dsn, "5.2.") { // mailbox full / other — transient
		return ClassSoft
	}

	// No conclusive DSN — fall back to the PMTA category.
	switch bounceCat {
	case "spam-related", "policy-related", "routing-errors":
		return ClassBlock
	case "no-answer-from-host", "bad-connection", "quota-issues":
		return ClassSoft
	}
	if isGenuineHardCategory(bounceCat) {
		return ClassHard
	}
	return ClassSoft
}

// ClassifyError examines an SMTP error string and returns hard or soft. A
// reputation/system/policy block (5.3/5.4/5.5/5.7) is treated as SOFT so the
// send path never permanently suppresses a valid recipient over a sender-side
// block. A nil error returns BounceSoft.
func ClassifyError(err error) BounceType {
	if err == nil {
		return BounceSoft
	}
	msg := err.Error()
	switch ClassifyBounce(ExtractDSN(msg), "", msg) {
	case ClassHard:
		return BounceHard
	case ClassBlock:
		return BounceSoft
	}
	// No conclusive enhanced DSN code: fall back to the bare 5xx reply code so a
	// genuine permanent failure ("550 mailbox unavailable") is still hard.
	if ExtractDSN(msg) == "" && replyCodeRe.MatchString(msg) {
		return BounceHard
	}
	return BounceSoft
}
