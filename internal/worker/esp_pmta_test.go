package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVMTAShortName(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		want     string
	}{
		{"full FQDN", "mta1.mail.projectjarvis.io", "mta1"},
		{"two-part hostname", "mta2.example.com", "mta2"},
		{"already short", "mta3", "mta3"},
		{"empty string", "", ""},
		{"single char prefix", "m.example.com", "m"},
		{"ip-like hostname", "15.204.101.125", "15"},
		{"dash in prefix", "mta-1.mail.example.com", "mta-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := vmtaShortName(tc.hostname)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestVMTAShortName_ValidationRules(t *testing.T) {
	badHostnames := []struct {
		hostname string
		reason   string
	}{
		{"", "empty hostname produces empty VMTA"},
		{"a.b.c", "single-char VMTA 'a' may not match any directive"},
	}

	for _, tc := range badHostnames {
		vmta := vmtaShortName(tc.hostname)
		if vmta != "" && len(vmta) < 2 {
			t.Logf("WARNING: hostname %q produces short VMTA %q — %s", tc.hostname, vmta, tc.reason)
		}
	}
}

// ---------------------------------------------------------------------------
// vmtaPool.next() — ISP-aware selection
// ---------------------------------------------------------------------------

func buildTestPool(groups map[string][]vmtaEntry, flatIPs []vmtaEntry, prefix string) *vmtaPool {
	idxMap := make(map[string]*uint64, len(groups))
	for k := range groups {
		v := uint64(0)
		idxMap[k] = &v
	}
	return &vmtaPool{
		mu:         sync.RWMutex{},
		ips:        flatIPs,
		ispGroups:  groups,
		ispIdx:     idxMap,
		poolPrefix: prefix,
	}
}

func TestVMTAPoolNext_ISPSpecificSelection(t *testing.T) {
	gmailIPs := []vmtaEntry{
		{ID: "gm1", Hostname: "mta-db-gm1.mail.em.discountblog.com", IP: "144.225.178.7", Status: "warmup", WarmupDailyLimit: 100, TodaySent: 0},
		{ID: "gm2", Hostname: "mta-db-gm2.mail.em.discountblog.com", IP: "144.225.178.8", Status: "warmup", WarmupDailyLimit: 100, TodaySent: 0},
	}
	yahooIPs := []vmtaEntry{
		{ID: "yh1", Hostname: "mta-a-shrd1.mail.em.discountblog.com", IP: "15.204.22.177", Status: "warmup", WarmupDailyLimit: 100, TodaySent: 0},
	}
	generalIPs := []vmtaEntry{
		{ID: "gn1", Hostname: "mta-db-gn1.mail.em.discountblog.com", IP: "144.225.178.10", Status: "warmup", WarmupDailyLimit: 100, TodaySent: 0},
	}

	allIPs := append(append(gmailIPs, yahooIPs...), generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"gmail":   gmailIPs,
		"yahoo":   yahooIPs,
		"general": generalIPs,
	}, allIPs, "db")

	ip, err := pool.next("gmail")
	require.NoError(t, err)
	assert.Contains(t, ip.ID, "gm", "gmail ISP should route to gmail IPs")

	ip, err = pool.next("yahoo")
	require.NoError(t, err)
	assert.Equal(t, "yh1", ip.ID)
}

func TestVMTAPoolNext_FallbackToGeneral(t *testing.T) {
	generalIPs := []vmtaEntry{
		{ID: "gn1", Hostname: "mta-db-gn1.mail.em.discountblog.com", Status: "warmup", WarmupDailyLimit: 100, TodaySent: 0},
	}

	pool := buildTestPool(map[string][]vmtaEntry{
		"general": generalIPs,
	}, generalIPs, "db")

	ip, err := pool.next("verizon")
	require.NoError(t, err)
	assert.Equal(t, "gn1", ip.ID, "unmapped ISP should fall back to general pool")
}

// ---------------------------------------------------------------------------
// THE IP LAYER DOES NOT JUDGE VOLUME (operator ruling 2026-09-09).
//
// These four tests replace the pre-2026-09-09 set, which asserted the opposite:
// that an IP at warmup_daily_limit was skipped, that a fully-consumed ISP group
// fell through to general, and that a fully-consumed pool returned
// "all IPs exhausted". That gate dead-lettered 26,757 contract-approved
// messages on 2026-09-08 and 25,593 on 2026-09-09. Volume is decided upstream
// (drip supply contract / campaign quota); warmup_daily_limit and TodaySent are
// accounting and log fields.
// ---------------------------------------------------------------------------

func TestVMTAPoolNext_IPAtLimitIsStillSelected(t *testing.T) {
	// gm1 is at its limit, gm2 is over it. Both remain selectable, and the
	// round robin still visits both.
	gmailIPs := []vmtaEntry{
		{ID: "gm1", Hostname: "mta-db-gm1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 50},
		{ID: "gm2", Hostname: "mta-db-gm2", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 5000},
	}
	generalIPs := []vmtaEntry{
		{ID: "gn1", Hostname: "mta-db-gn1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 10},
	}

	allIPs := append(append([]vmtaEntry{}, gmailIPs...), generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"gmail":   gmailIPs,
		"general": generalIPs,
	}, allIPs, "db")

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		ip, err := pool.next("gmail")
		require.NoError(t, err, "an at/over-limit IP must never produce an error")
		seen[ip.ID]++
	}
	assert.Equal(t, 2, seen["gm1"], "gm1 is AT its limit and must still take its turn")
	assert.Equal(t, 2, seen["gm2"], "gm2 is 100x OVER its limit and must still take its turn")
	assert.Zero(t, seen["gn1"], "the gmail group is non-empty, so general is never reached")
}

func TestVMTAPoolNext_ISPGroupAtLimit_StaysOnItsOwnGroup(t *testing.T) {
	// Pre-change this fell through to general "because gmail was exhausted".
	// Falling through on volume is itself the bug: it mails gmail traffic from
	// the general IPs, which is the routing the ISP groups exist to prevent.
	gmailIPs := []vmtaEntry{
		{ID: "gm1", Hostname: "mta-db-gm1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 50},
	}
	generalIPs := []vmtaEntry{
		{ID: "gn1", Hostname: "mta-db-gn1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 10},
	}

	allIPs := append(append([]vmtaEntry{}, gmailIPs...), generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"gmail":   gmailIPs,
		"general": generalIPs,
	}, allIPs, "db")

	ip, err := pool.next("gmail")
	require.NoError(t, err)
	assert.Equal(t, "gm1", ip.ID, "an at-limit ISP group keeps its own ISP's traffic")
}

func TestVMTAPoolNext_EveryIPOverLimit_StillReturnsAnIP(t *testing.T) {
	// The old TestVMTAPoolNext_AllExhausted_ReturnsError asserted an error here.
	// The new contract: there is no volume-based exit from next().
	gmailIPs := []vmtaEntry{
		{ID: "gm1", Hostname: "mta-db-gm1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 999999},
	}
	generalIPs := []vmtaEntry{
		{ID: "gn1", Hostname: "mta-db-gn1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 999999},
	}

	allIPs := append(append([]vmtaEntry{}, gmailIPs...), generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"gmail":   gmailIPs,
		"general": generalIPs,
	}, allIPs, "db")

	for i := 0; i < 50; i++ {
		ip, err := pool.next("gmail")
		require.NoError(t, err, "call %d: a saturated pool must still hand back an IP", i)
		assert.Equal(t, "gm1", ip.ID)
	}
}

func TestVMTAPoolNext_ZeroWarmupLimit_StillSelected(t *testing.T) {
	// warmup_daily_limit = 0 was the hardest form of the old refusal: every
	// send failed because TodaySent >= 0 is always true.
	ips := []vmtaEntry{
		{ID: "yh1", Hostname: "mta-db-yh1", Status: "warmup", WarmupDailyLimit: 0, TodaySent: 0},
	}
	pool := buildTestPool(map[string][]vmtaEntry{"yahoo": ips}, ips, "db")

	ip, err := pool.next("yahoo")
	require.NoError(t, err)
	assert.Equal(t, "yh1", ip.ID)
}

func TestVMTAPoolNext_RoundRobin(t *testing.T) {
	gmailIPs := []vmtaEntry{
		{ID: "gm1", Hostname: "mta-db-gm1", Status: "active", WarmupDailyLimit: 10000},
		{ID: "gm2", Hostname: "mta-db-gm2", Status: "active", WarmupDailyLimit: 10000},
		{ID: "gm3", Hostname: "mta-db-gm3", Status: "active", WarmupDailyLimit: 10000},
	}

	pool := buildTestPool(map[string][]vmtaEntry{
		"gmail": gmailIPs,
	}, gmailIPs, "db")

	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		ip, err := pool.next("gmail")
		require.NoError(t, err)
		seen[ip.ID]++
	}
	assert.Equal(t, 3, seen["gm1"], "round-robin should distribute evenly")
	assert.Equal(t, 3, seen["gm2"])
	assert.Equal(t, 3, seen["gm3"])
}

func TestVMTAPoolNext_LegacyNoISPGroups(t *testing.T) {
	flatIPs := []vmtaEntry{
		{ID: "ip1", Hostname: "mta2.mail.projectjarvis.io", Status: "warmup", WarmupDailyLimit: 10000, TodaySent: 0},
		{ID: "ip2", Hostname: "mta3.mail.projectjarvis.io", Status: "warmup", WarmupDailyLimit: 10000, TodaySent: 0},
	}

	pool := buildTestPool(map[string][]vmtaEntry{}, flatIPs, "")

	ip, err := pool.next("gmail")
	require.NoError(t, err)
	assert.Contains(t, []string{"ip1", "ip2"}, ip.ID, "legacy path should return from flat list")
}

func TestVMTAPoolNext_EmptyPool(t *testing.T) {
	pool := buildTestPool(map[string][]vmtaEntry{}, nil, "db")
	_, err := pool.next("gmail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no IPs in pool")
}

// ---------------------------------------------------------------------------
// ISP routing — all ISPs use standard Tier1 → Tier2 → Tier3 fallback
// ---------------------------------------------------------------------------

func TestVMTAPoolNext_ATTRoutesToATTGroup(t *testing.T) {
	attIPs := []vmtaEntry{
		{ID: "att1", Hostname: "mta-db-att1.mail.em.discountblog.com", IP: "144.225.178.11", Status: "active", WarmupDailyLimit: 10000},
	}
	generalIPs := []vmtaEntry{
		{ID: "gen1", Hostname: "mta-db-gn1.mail.em.discountblog.com", IP: "144.225.178.10", Status: "active", WarmupDailyLimit: 10000},
	}
	allIPs := append(attIPs, generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"att":     attIPs,
		"general": generalIPs,
	}, allIPs, "db")

	ip, err := pool.next("att")
	require.NoError(t, err)
	assert.Equal(t, "att1", ip.ID, "ATT should route to its own ISP group")
}

func TestVMTAPoolNext_CoxRoutesToCoxGroup(t *testing.T) {
	coxIPs := []vmtaEntry{
		{ID: "cox1", Hostname: "mta-db-cox1.mail.em.discountblog.com", IP: "144.225.178.12", Status: "active", WarmupDailyLimit: 10000},
	}
	generalIPs := []vmtaEntry{
		{ID: "gen1", Hostname: "mta-db-gn1.mail.em.discountblog.com", IP: "144.225.178.10", Status: "active", WarmupDailyLimit: 10000},
	}
	allIPs := append(coxIPs, generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"cox":     coxIPs,
		"general": generalIPs,
	}, allIPs, "db")

	ip, err := pool.next("cox")
	require.NoError(t, err)
	assert.Equal(t, "cox1", ip.ID, "Cox should route to its own ISP group")
}

func TestVMTAPoolNext_YahooRoutesToYahooGroup(t *testing.T) {
	yahooIPs := []vmtaEntry{
		{ID: "yh1", Hostname: "mta-db-yh1.mail.em.discountblog.com", IP: "144.225.178.7", Status: "active", WarmupDailyLimit: 10000},
	}
	generalIPs := []vmtaEntry{
		{ID: "gen1", Hostname: "mta-db-gn1.mail.em.discountblog.com", IP: "144.225.178.10", Status: "active", WarmupDailyLimit: 10000},
	}
	allIPs := append(yahooIPs, generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"yahoo":   yahooIPs,
		"general": generalIPs,
	}, allIPs, "db")

	ip, err := pool.next("yahoo")
	require.NoError(t, err)
	assert.Equal(t, "yh1", ip.ID, "Yahoo should use yahoo group")
}

func TestVMTAPoolNext_YahooAtLimit_StaysOnYahooGroup(t *testing.T) {
	// Was TestVMTAPoolNext_YahooExhausted_FallsBackToGeneral. Yahoo traffic
	// leaving the yahoo IPs because those IPs "used up their day" is exactly
	// the cross-lane leak the per-domain yahoo sets were built to stop.
	yahooIPs := []vmtaEntry{
		{ID: "yh1", Hostname: "mta-db-yh1.mail.em.discountblog.com", IP: "144.225.178.7", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 50},
	}
	generalIPs := []vmtaEntry{
		{ID: "gen1", Hostname: "mta-db-gn1.mail.em.discountblog.com", IP: "144.225.178.10", Status: "active", WarmupDailyLimit: 10000},
	}
	allIPs := append(append([]vmtaEntry{}, yahooIPs...), generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"yahoo":   yahooIPs,
		"general": generalIPs,
	}, allIPs, "db")

	ip, err := pool.next("yahoo")
	require.NoError(t, err)
	assert.Equal(t, "yh1", ip.ID, "yahoo stays on its own IPs regardless of TodaySent")
}

// ---------------------------------------------------------------------------
// Membership refusals that SURVIVE — these are not volume judgments.
// ---------------------------------------------------------------------------

func TestVMTAPoolNext_StrictPoolWithNoISPMembers_StillRefuses(t *testing.T) {
	// A strict-isolation pool that holds no IP OF THAT ISP has no correct IP to
	// send from. That is membership, and the send worker DEFERS it
	// (deferred_strict_pool → deferStrictPool's bounded backoff), it does not
	// dead-letter it. Note the IPs present are all over their limits: the
	// refusal must be attributable to membership, never to volume.
	generalIPs := []vmtaEntry{
		{ID: "gn1", Hostname: "mta-db-gn1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 999999},
	}
	pool := buildTestPool(map[string][]vmtaEntry{"general": generalIPs}, generalIPs, "db")
	pool.strictPools = map[string]bool{"yahoo": true}

	_, err := pool.next("yahoo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strict_pool_exhausted")
}

func TestVMTAPoolNext_StrictPoolWithMembers_SelectsThemAtAnyVolume(t *testing.T) {
	yahooIPs := []vmtaEntry{
		{ID: "yh1", Hostname: "mta-db-yh1", Status: "warmup", WarmupDailyLimit: 10, TodaySent: 999999},
	}
	pool := buildTestPool(map[string][]vmtaEntry{"yahoo": yahooIPs}, yahooIPs, "db")
	pool.strictPools = map[string]bool{"yahoo": true}

	ip, err := pool.next("yahoo")
	require.NoError(t, err, "a strict pool WITH an IP of that ISP never refuses on volume")
	assert.Equal(t, "yh1", ip.ID)
}

// ---------------------------------------------------------------------------
// NEGATIVE CONTROL — no code path in these two files may refuse on volume.
// ---------------------------------------------------------------------------

// TestNoVolumeRefusalInPMTASenders is a source-level pin. A behavioral test can
// only prove the paths it walks; this proves the gate cannot be reintroduced in
// any branch, including ones no test constructs. It fails on pre-change HEAD.
func TestNoVolumeRefusalInPMTASenders(t *testing.T) {
	forbidden := []struct{ pattern, why string }{
		{"exhausted warmup limits", "the 2026-09-08/09 dead-letter string — the IP layer must not refuse on volume"},
		{"all IPs exhausted", "a volume-shaped refusal from vmtaPool.next"},
		{"TodaySent >", "a per-IP volume gate"},
		{"TodaySent <", "a per-IP volume gate"},
		{"TodaySent =", "a per-IP volume gate"},
		{"int64(ip.WarmupDailyLimit)", "the exact conversion the removed gate used to compare against TodaySent"},
	}
	for _, path := range []string{"esp_pmta.go", "esp_pmta_api.go"} {
		src, err := os.ReadFile(path)
		require.NoError(t, err, "read %s", path)
		for _, f := range forbidden {
			// The log line prints both counters; that is accounting, and it is
			// the only permitted mention.
			for _, line := range strings.Split(string(src), "\n") {
				if strings.Contains(line, "log.Printf") || strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				assert.NotContains(t, line, f.pattern,
					"%s: %q reappeared — %s", path, f.pattern, f.why)
			}
		}
	}
}

// TestVMTAPoolNext_NeverErrorsOnVolume walks every tier of the fallback chain
// with a pool whose IPs are all far past their limits, and asserts that no
// call errors and that no error text mentions exhaustion.
func TestVMTAPoolNext_NeverErrorsOnVolume(t *testing.T) {
	over := func(id string) vmtaEntry {
		return vmtaEntry{ID: id, Hostname: "mta-db-" + id, Status: "warmup", WarmupDailyLimit: 1, TodaySent: 1_000_000}
	}
	cases := []struct {
		name string
		pool *vmtaPool
		isp  string
	}{
		{
			name: "tier1 isp group",
			pool: buildTestPool(map[string][]vmtaEntry{"gmail": {over("gm1"), over("gm2")}}, []vmtaEntry{over("gm1"), over("gm2")}, "db"),
			isp:  "gmail",
		},
		{
			name: "tier2 general fallback",
			pool: buildTestPool(map[string][]vmtaEntry{"general": {over("gn1")}}, []vmtaEntry{over("gn1")}, "db"),
			isp:  "gmail",
		},
		{
			name: "tier3 flat list",
			pool: buildTestPool(map[string][]vmtaEntry{}, []vmtaEntry{over("f1"), over("f2")}, ""),
			isp:  "gmail",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 25; i++ {
				ip, err := tc.pool.next(tc.isp)
				if err != nil {
					t.Fatalf("call %d refused a send: %v", i, err)
				}
				require.NotEmpty(t, ip.ID)
			}
		})
	}
}

// TestPMTASenders_StrictPoolMissDefersNotDeadLetters pins that BOTH senders map
// a strict-pool membership miss to deferred_strict_pool, which send_worker.go
// routes to deferStrictPool (bounded backoff, then a dead_letter_strict WITH an
// operator alert). The API sender used to flatten it into "no sending IPs
// configured", which markFailed treats as terminal and the journey retry
// classifier reads as grounds to eject the enrollment.
func TestPMTASenders_StrictPoolMissDefersNotDeadLetters(t *testing.T) {
	newStrictPool := func() *vmtaPool {
		ips := []vmtaEntry{{ID: "gn1", Hostname: "mta-db-gn1", Status: "active", WarmupDailyLimit: 10}}
		p := buildTestPool(map[string][]vmtaEntry{"general": ips}, ips, "db")
		p.strictPools = map[string]bool{"yahoo": true}
		// Non-zero loadedAt + long TTL keeps refresh() from touching a nil DB.
		p.loadedAt = time.Now()
		p.ttl = time.Hour
		return p
	}
	msg := &EmailMessage{
		Email: "user@yahoo.com", FromName: "Test", FromEmail: "test@em.discountblog.com",
		Subject: "Hello", HTMLContent: "<p>Test</p>", ProfileID: "prof-1", RecipientISP: "yahoo",
		Headers: map[string]string{},
	}

	t.Run("api sender", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("bridge must not be called when no IP is available")
			w.WriteHeader(200)
		}))
		defer srv.Close()
		s := NewPMTAAPISender(srv.URL, nil, "db")
		s.ipPool = newStrictPool()

		_, err := s.Send(context.Background(), msg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "deferred_strict_pool")
		assert.NotContains(t, err.Error(), "exhausted warmup limits")
	})

	t.Run("smtp sender", func(t *testing.T) {
		s := NewPMTASender("127.0.0.1", 2525, "u", "p", nil, "db")
		s.ipPool = newStrictPool()

		_, err := s.Send(context.Background(), msg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "deferred_strict_pool")
		assert.NotContains(t, err.Error(), "exhausted warmup limits")
	})
}

func TestVMTAPoolNext_UnmappedISP_FallsBackToGeneral(t *testing.T) {
	generalIPs := []vmtaEntry{
		{ID: "gen1", Hostname: "mta-db-gn1.mail.em.discountblog.com", IP: "144.225.178.10", Status: "active", WarmupDailyLimit: 10000},
	}
	pool := buildTestPool(map[string][]vmtaEntry{
		"general": generalIPs,
	}, generalIPs, "db")

	ip, err := pool.next("verizon")
	require.NoError(t, err)
	assert.Equal(t, "gen1", ip.ID, "unmapped ISPs should fall back to general")
}
