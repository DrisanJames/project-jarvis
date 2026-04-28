package worker

import "strings"

// IsPMTATransient classifies an error message string as an infra-transient
// failure originating from the local PMTA path (HTTP bridge, SMTP submission,
// network to PMTA, DNS to bridge host) rather than from a recipient ISP.
//
// Infra-transient errors mean "PMTA itself is bouncing" — typically a pmtad
// crash/restart window (~30s), a resolver thread stall, or the bridge being
// briefly unreachable. These are not the recipient's fault and must NOT
// burn the recipient's retry budget.
//
// Recipient-attributable errors (550 mailbox doesn't exist, 5xx policy
// rejections, DKIM/SPF failures from a destination ISP) keep the existing
// markFailed behavior — they legitimately count as attempts.
//
// Note on 421: in this codebase a 421 in error_message originates from the
// pmta-http-bridge wrapping a local SMTP 4xx into HTTP 502. Recipient-side
// 421 throttling never surfaces here — it is absorbed by PMTA's own queue
// and surfaces later as a hard bounce or a successful send after backoff.
// Treating "421" as infra-transient is therefore correct for this code path.
func IsPMTATransient(errMsg string) bool {
	s := strings.ToLower(errMsg)
	for _, p := range pmtaTransientPatterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// pmtaTransientPatterns is the canonical list of substrings that identify
// a PMTA / bridge / network-to-bridge transient failure. Order is not
// significant — IsPMTATransient stops on first match.
var pmtaTransientPatterns = []string{
	"connection refused",
	"connection reset by peer",
	"connection unexpectedly closed",
	"broken pipe",
	"eof",
	"i/o timeout",
	"context deadline exceeded",
	"client.timeout exceeded", // Go stdlib net/http: "Client.Timeout exceeded while awaiting headers"
	"server closed idle connection",
	"no such host",
	"temporary failure in name resolution",
	"pmta api error",
	"http 500",
	"http 502",
	"http 503",
	"http 504",
	"service unavailable",
	"421",
}

// PMTATransientBackoffMinutes is the cooldown applied to a recipient that
// hit a PMTA-local transient failure. Flat 10 minutes — long enough to
// ride out a pmtad crash/restart cycle (typical ~30s) plus resolver-stall
// recovery without retrying so fast that we pile attempts onto a still-
// bouncing bridge.
const PMTATransientBackoffMinutes = 10
