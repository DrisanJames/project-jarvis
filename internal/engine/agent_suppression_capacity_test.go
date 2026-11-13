package engine

import "testing"

// A capacity deferral must never feed the repeated-transient counter: Yahoo TSS04,
// SES 454 throttling, relay timeouts and dropped sessions describe the path, not
// the mailbox. 2026-09-01..03 this rule suppressed 12.7k engaged yahoo openers.
func TestIsCapacityTransient(t *testing.T) {
	yes := []string{
		"smtp;421 4.7.0 [TSS04] Messages from 16.217.96.5 temporarily deferred due to unexpected volume or user complaints",
		"smtp;454 Throttling failure: Maximum sending rate exceeded.",
		"smtp;451 4.4.2 Timeout waiting for data from client.",
		"KumoMTA internal: failed to send message: Error Connection closed by peer reading response to cmd=RSET",
		"KumoMTA internal: no sources for x@yahoo.com pool=`mta-aad-yh2` are eligible for selection at this time",
		"smtp;421 4.7.0 [TSS03] All messages from 1.2.3.4 will be permanently deferred",
		// Microsoft Exchange Online incident 2026-11-12/13 — provider front door, not the mailbox (brain #30723)
		"smtp;451 4.7.0 Temporary server error. Please try again later. PRX3 NextHop: substrate-int.office.com. Last IP: 2603:10b6:3d0::1",
		"Temporary server error. Please try again later. PRX5 NextHop: substrate-int.office.com. Next Hop IP: 1.2.3.4",
		"KumoMTA internal: failed to send message: Error Connection reset by peer (os error 104) reading response to cmd=DATA",
		"KumoMTA internal: failed to send message: Error Broken pipe (os error 32) writing RSET",
		"KumoMTA internal: failed to connect to any candidate hosts: connect to 1.2.3.4:25 and read initial banner: Command timeout",
	}
	for _, d := range yes {
		if !isCapacityTransient(d) {
			t.Errorf("expected capacity transient: %q", d)
		}
	}
	no := []string{
		"smtp;452 4.2.2 Mailbox full",
		"smtp;450 4.1.1 User unknown, try later",
		"",
	}
	for _, d := range no {
		if isCapacityTransient(d) {
			t.Errorf("must NOT be capacity transient: %q", d)
		}
	}
}
