package worker

import (
	"sync"
	"testing"

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

func TestVMTAPoolNext_WarmupLimitEnforcement(t *testing.T) {
	gmailIPs := []vmtaEntry{
		{ID: "gm1", Hostname: "mta-db-gm1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 50},
		{ID: "gm2", Hostname: "mta-db-gm2", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 49},
	}
	generalIPs := []vmtaEntry{
		{ID: "gn1", Hostname: "mta-db-gn1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 10},
	}

	allIPs := append(gmailIPs, generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"gmail":   gmailIPs,
		"general": generalIPs,
	}, allIPs, "db")

	ip, err := pool.next("gmail")
	require.NoError(t, err)
	assert.Equal(t, "gm2", ip.ID, "should skip gm1 (at limit) and select gm2 (under limit)")
}

func TestVMTAPoolNext_AllISPExhausted_FallsBackToGeneral(t *testing.T) {
	gmailIPs := []vmtaEntry{
		{ID: "gm1", Hostname: "mta-db-gm1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 50},
	}
	generalIPs := []vmtaEntry{
		{ID: "gn1", Hostname: "mta-db-gn1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 10},
	}

	allIPs := append(gmailIPs, generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"gmail":   gmailIPs,
		"general": generalIPs,
	}, allIPs, "db")

	ip, err := pool.next("gmail")
	require.NoError(t, err)
	assert.Equal(t, "gn1", ip.ID, "when all gmail IPs exhausted, should fall to general")
}

func TestVMTAPoolNext_AllExhausted_ReturnsError(t *testing.T) {
	gmailIPs := []vmtaEntry{
		{ID: "gm1", Hostname: "mta-db-gm1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 50},
	}
	generalIPs := []vmtaEntry{
		{ID: "gn1", Hostname: "mta-db-gn1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 50},
	}

	allIPs := append(gmailIPs, generalIPs...)
	pool := buildTestPool(map[string][]vmtaEntry{
		"gmail":   gmailIPs,
		"general": generalIPs,
	}, allIPs, "db")

	_, err := pool.next("gmail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all IPs exhausted")
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

func TestVMTAPoolNext_YahooExhausted_FallsBackToGeneral(t *testing.T) {
	yahooIPs := []vmtaEntry{
		{ID: "yh1", Hostname: "mta-db-yh1.mail.em.discountblog.com", IP: "144.225.178.7", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 50},
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
	assert.Equal(t, "gen1", ip.ID, "when yahoo IPs exhausted, should fall back to general")
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
