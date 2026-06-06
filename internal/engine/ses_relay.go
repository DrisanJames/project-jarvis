package engine

import "strings"

// IsPMTARelayedToSES reports whether a PMTA accounting record describes a
// message that PMTA handed off to AWS SES (rather than delivering directly to
// the recipient MX). For these records a PMTA "d" (delivery) event means
// "relayed to SES", NOT "delivered to the recipient's mail system" — the
// authoritative delivery signal for SES-routed mail is the SES DELIVERY event
// published through the configuration set (persisted via the SES events
// webhook). Counting PMTA's relay-success as a campaign "delivered" is exactly
// what made Campaign Center report 100% delivered while SES VDM reported ~72%.
//
// Detection keys off the VMTA / pool name because the accounting webhook has no
// sending-profile context. SES routes use pool/VMTA names that contain "ses":
//   - tenant cutover (Jun 2026): pools db-ses-pool / ht-ses-pool / qf-ses-pool /
//     mh-ses-pool / rr-ses-pool, VMTAs vmta-db-ses / vmta-ht-ses / ...
//   - legacy m.* relay: pool ses-relay-a / ses-relay-b, VMTA ses-relay
//
// Using both pool and vmta covers accounting lines that populate only one of
// the two fields.
func IsPMTARelayedToSES(rec AccountingRecord) bool {
	return strings.Contains(strings.ToLower(rec.Pool), "ses") ||
		strings.Contains(strings.ToLower(rec.VMTA), "ses")
}
