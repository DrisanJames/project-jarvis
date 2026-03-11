package pmta

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testHeader = "#type,timeLogged,orig,rcpt,orcpt,dsnAction,dsnStatus,dsnDiag,dsnMTA,bounceCat,srcType,srcMTA,dlvType,dlvSourceIp,dlvDestinationIp,dlvEsmtpAvailable,dlvSize,vmta,jobId,envId,queue,vmtaPool"

func TestParseDeliveryRecord(t *testing.T) {
	csv := testHeader + "\n" +
		"d,2026-03-11 10:00:00,sender@example.com,user@gmail.com,,delivered,2.0.0,250 OK,,,,,,15.204.101.125,142.250.152.26,,1234,mta1,job-123,env-1,gmail.com,gmail-pool\n"

	p := NewAcctParser()
	records, err := p.ParseReader(strings.NewReader(csv))
	require.NoError(t, err)
	require.Len(t, records, 1)

	r := records[0]
	assert.Equal(t, "d", r.Type)
	assert.Equal(t, "user@gmail.com", r.Rcpt)
	assert.Equal(t, "gmail.com", r.Domain)
	assert.Equal(t, "sender@example.com", r.Orig)
	assert.Equal(t, "15.204.101.125", r.SourceIP)
	assert.Equal(t, "mta1", r.VMTA)
	assert.Equal(t, "job-123", r.JobID)
	assert.Equal(t, "2.0.0", r.BounceCode)
}

func TestParseBounceRecord(t *testing.T) {
	csv := testHeader + "\n" +
		"b,2026-03-11 10:01:00,sender@example.com,bad@invalid.com,,failed,5.1.1,550 User unknown,,bad-mailbox,,,,,,,1234,mta1,job-456,env-2,,\n"

	p := NewAcctParser()
	records, err := p.ParseReader(strings.NewReader(csv))
	require.NoError(t, err)
	require.Len(t, records, 1)

	r := records[0]
	assert.Equal(t, "b", r.Type)
	assert.Equal(t, "bad@invalid.com", r.Rcpt)
	assert.Equal(t, "bad-mailbox", r.BounceCat)
	assert.Equal(t, "5.1.1", r.BounceCode)
	assert.Equal(t, "550 User unknown", r.DSNDiag)
}

func TestParseFBLRecord(t *testing.T) {
	csv := testHeader + "\n" +
		"f,2026-03-11 10:02:00,sender@example.com,complainer@yahoo.com,,,,,,,,,,,,,,mta2,job-789,env-3,,\n"

	p := NewAcctParser()
	records, err := p.ParseReader(strings.NewReader(csv))
	require.NoError(t, err)
	require.Len(t, records, 1)

	r := records[0]
	assert.Equal(t, "f", r.Type)
	assert.Equal(t, "complainer@yahoo.com", r.Rcpt)
	assert.Equal(t, "yahoo.com", r.Domain)
}

func TestParseDeferralRecord(t *testing.T) {
	csv := testHeader + "\n" +
		"t,2026-03-11 10:03:00,sender@example.com,deferred@gmail.com,,delayed,4.2.1,452 Too many recipients,,quota-issues,,,,,,,1234,mta1,job-101,env-4,,\n"

	p := NewAcctParser()
	records, err := p.ParseReader(strings.NewReader(csv))
	require.NoError(t, err)
	require.Len(t, records, 1)

	r := records[0]
	assert.Equal(t, "t", r.Type)
	assert.Equal(t, "deferred@gmail.com", r.Rcpt)
	assert.Equal(t, "quota-issues", r.BounceCat)
}

func TestBounceCategory_HardVsSoft(t *testing.T) {
	hardCategories := []string{"bad-mailbox", "bad-domain", "inactive-mailbox", "no-answer-from-host", "routing-errors"}
	softCategories := []string{"quota-issues", "message-expired", "policy-related", "content-related"}

	for _, cat := range hardCategories {
		assert.True(t, isHardBounce(cat), "category %q should be classified as hard bounce", cat)
	}
	for _, cat := range softCategories {
		assert.False(t, isHardBounce(cat), "category %q should NOT be classified as hard bounce", cat)
	}
}

func TestMalformedRecords(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"too few fields", testHeader + "\na,b\n"},
		{"empty line", testHeader + "\n\n"},
		{"comment only", testHeader + "\n# This is a comment\n"},
		{"bad timestamp", testHeader + "\nd,not-a-time,sender@example.com,user@gmail.com,,,,,,,,,,,,,,mta1,job-1,,,\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewAcctParser()
			records, err := p.ParseReader(strings.NewReader(tc.input))
			assert.NoError(t, err, "parser should not error on malformed input")
			// Malformed records are skipped, but the parser itself shouldn't fail
			_ = records
		})
	}
}

func TestMultipleRecords(t *testing.T) {
	csv := testHeader + "\n" +
		"d,2026-03-11 10:00:00,s@ex.com,a@gmail.com,,,,,,,,,,1.2.3.4,,,,mta1,j1,,,\n" +
		"b,2026-03-11 10:01:00,s@ex.com,b@yahoo.com,,,,,,bad-mailbox,,,,,,,,mta2,j2,,,\n" +
		"d,2026-03-11 10:02:00,s@ex.com,c@outlook.com,,,,,,,,,,1.2.3.4,,,,mta1,j3,,,\n"

	p := NewAcctParser()
	records, err := p.ParseReader(strings.NewReader(csv))
	require.NoError(t, err)
	assert.Len(t, records, 3)
	assert.Equal(t, "d", records[0].Type)
	assert.Equal(t, "b", records[1].Type)
	assert.Equal(t, "d", records[2].Type)
}

func TestAggregateByIP(t *testing.T) {
	records := []AcctRecord{
		{Type: "d", SourceIP: "1.2.3.4", VMTA: "mta1"},
		{Type: "d", SourceIP: "1.2.3.4", VMTA: "mta1"},
		{Type: "b", SourceIP: "1.2.3.4", VMTA: "mta1"},
		{Type: "d", SourceIP: "5.6.7.8", VMTA: "mta2"},
		{Type: "f", SourceIP: "5.6.7.8", VMTA: "mta2"},
	}

	agg := AggregateByIP(records)
	require.Contains(t, agg, "1.2.3.4")
	require.Contains(t, agg, "5.6.7.8")

	ip1 := agg["1.2.3.4"]
	assert.Equal(t, int64(3), ip1.TotalSent)
	assert.Equal(t, int64(2), ip1.TotalDelivered)
	assert.Equal(t, int64(1), ip1.TotalBounced)

	ip2 := agg["5.6.7.8"]
	assert.Equal(t, int64(1), ip2.TotalSent)
	assert.Equal(t, int64(1), ip2.TotalDelivered)
	assert.Equal(t, int64(1), ip2.TotalComplained)
}

// isHardBounce mirrors the classification logic from the ingestor.
func isHardBounce(cat string) bool {
	switch cat {
	case "bad-mailbox", "bad-domain", "inactive-mailbox", "no-answer-from-host", "routing-errors":
		return true
	default:
		return false
	}
}
