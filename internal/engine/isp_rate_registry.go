package engine

import (
	"context"
	"database/sql"
	"log"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// IPEntry describes a single IP that serves traffic for an ISP.
type IPEntry struct {
	Hostname         string
	Status           string // "active" or "warmup"
	WarmupDailyLimit int
}

// ISPRateRegistry is a thread-safe registry of per-ISP rate limiters.
// ThrottleAgents call SetRate when they compute new rates.
// The dispatch loop calls AllowN before claiming a batch for each ISP.
//
// When a *sql.DB is attached via SetDB, rate changes are persisted to
// mailing_isp_throttle_state so they survive server restarts.
//
// Per-IP rate limiting: when IP lists are set via SetIPList, per-IP limiters
// are created that subdivide the ISP rate across IPs (warmup-aware).
type ISPRateRegistry struct {
	mu          sync.RWMutex
	limiters    map[ISP]*rate.Limiter
	rates       map[ISP]float64 // current msgs/hour for logging
	db          *sql.DB
	shutdownCtx context.Context // server lifecycle context for persist goroutines

	ipLimiters map[string]*rate.Limiter // keyed by "isp:hostname"
	ipRates    map[string]float64       // keyed by "isp:hostname"
	ipLists    map[ISP][]IPEntry        // which IPs serve each ISP
}

// staleness threshold: persisted rates older than this are ignored on restore
// because ISP conditions likely changed while the server was down.
const throttleStateTTL = 1 * time.Hour

// NewISPRateRegistry creates an empty registry. Call SetRate for each ISP
// to populate it with initial rates before use.
func NewISPRateRegistry() *ISPRateRegistry {
	return &ISPRateRegistry{
		limiters:   make(map[ISP]*rate.Limiter),
		rates:      make(map[ISP]float64),
		ipLimiters: make(map[string]*rate.Limiter),
		ipRates:    make(map[string]float64),
		ipLists:    make(map[ISP][]IPEntry),
	}
}

// SetDB attaches a database connection for rate persistence.
func (r *ISPRateRegistry) SetDB(db *sql.DB) {
	r.db = db
}

// SetShutdownContext provides the server lifecycle context so persist
// goroutines are cancelled on shutdown rather than orphaned.
func (r *ISPRateRegistry) SetShutdownContext(ctx context.Context) {
	r.shutdownCtx = ctx
}

func (r *ISPRateRegistry) persistCtx(timeout time.Duration) (context.Context, context.CancelFunc) {
	parent := r.shutdownCtx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}

// setRateInternal updates the in-memory limiter and rate map without
// touching the database. Used by RestoreFromDB to avoid writing back
// the same value that was just read.
func (r *ISPRateRegistry) setRateInternal(isp ISP, msgsPerHour float64) {
	perSecond := msgsPerHour / 3600.0
	burst := int(math.Max(1, msgsPerHour/360.0))

	r.mu.Lock()
	if lim, ok := r.limiters[isp]; ok {
		lim.SetLimit(rate.Limit(perSecond))
		lim.SetBurst(burst)
	} else {
		r.limiters[isp] = rate.NewLimiter(rate.Limit(perSecond), burst)
	}
	r.rates[isp] = msgsPerHour
	r.mu.Unlock()
}

// SetRate sets or updates the rate limit for an ISP.
// msgsPerHour is the sustained sending rate (e.g., 500 means 500 messages/hour).
// The burst is set to ~10 seconds of budget (msgsPerHour/360), minimum 1.
// If a DB is attached, the rate is persisted asynchronously.
func (r *ISPRateRegistry) SetRate(isp ISP, msgsPerHour float64) {
	r.setRateInternal(isp, msgsPerHour)
	r.rebalanceIPs(isp)

	if r.db != nil {
		go r.persistRate(isp, msgsPerHour)
	}
}

// persistRate writes one ISP's rate to the DB. Runs in a background goroutine
// so SetRate never blocks on I/O.
func (r *ISPRateRegistry) persistRate(isp ISP, msgsPerHour float64) {
	ctx, cancel := r.persistCtx(3 * time.Second)
	defer cancel()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO mailing_isp_throttle_state (isp, msgs_per_hour, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (isp) DO UPDATE SET msgs_per_hour = $2, updated_at = NOW()
	`, string(isp), msgsPerHour)
	if err != nil {
		log.Printf("[rate-registry] persist %s rate %.0f/hr failed: %v", isp, msgsPerHour, err)
	}
}

// RestoreFromDB loads persisted throttle rates, applying only rates that are
// lower than the provided config defaults (i.e., the ISP was being throttled).
// Stale entries older than throttleStateTTL are ignored.
func (r *ISPRateRegistry) RestoreFromDB(configRates map[ISP]float64) int {
	if r.db == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := r.db.QueryContext(ctx, `
		SELECT isp, msgs_per_hour, updated_at
		FROM mailing_isp_throttle_state
		WHERE updated_at > $1
	`, time.Now().Add(-throttleStateTTL))
	if err != nil {
		log.Printf("[rate-registry] restore from DB failed: %v", err)
		return 0
	}
	defer rows.Close()

	restored := 0
	for rows.Next() {
		var ispStr string
		var msgsPerHour float64
		var updatedAt time.Time
		if err := rows.Scan(&ispStr, &msgsPerHour, &updatedAt); err != nil {
			continue
		}
		isp := ISP(ispStr)
		configRate, hasConfig := configRates[isp]
		if !hasConfig {
			continue
		}
		if msgsPerHour >= configRate {
			continue
		}
		r.setRateInternal(isp, msgsPerHour)
		restored++
		log.Printf("[rate-registry] restored throttled rate for %s: %.0f/hr (config default: %.0f/hr, saved %s ago)",
			isp, msgsPerHour, configRate, time.Since(updatedAt).Truncate(time.Second))
	}
	return restored
}

// AllowN checks if the ISP's rate limiter can permit 'requested' messages.
// Returns how many of the requested messages are allowed (0 to requested).
// Unknown ISPs (not in the registry) are always fully permitted.
func (r *ISPRateRegistry) AllowN(isp string, requested int) int {
	if requested <= 0 {
		return 0
	}

	r.mu.RLock()
	lim, ok := r.limiters[ISP(isp)]
	r.mu.RUnlock()

	if !ok {
		return requested
	}

	now := time.Now()
	available := int(lim.TokensAt(now))
	if available <= 0 {
		return 0
	}

	n := requested
	if n > available {
		n = available
	}

	if lim.AllowN(now, n) {
		return n
	}
	return 0
}

// GetRate returns the current rate in msgs/hour for an ISP.
// Returns 0 if the ISP is not in the registry.
func (r *ISPRateRegistry) GetRate(isp ISP) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rates[isp]
}

// GetAllRates returns a snapshot of all current ISP rates in msgs/hour.
func (r *ISPRateRegistry) GetAllRates() map[ISP]float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[ISP]float64, len(r.rates))
	for k, v := range r.rates {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// Per-IP rate limiting
// ---------------------------------------------------------------------------

// SetIPList updates the set of IPs serving a given ISP and rebalances per-IP
// rate limiters. Called by vmtaPool.refresh when the IP set changes.
func (r *ISPRateRegistry) SetIPList(isp ISP, ips []IPEntry) {
	r.mu.Lock()
	r.ipLists[isp] = ips
	r.mu.Unlock()
	r.rebalanceIPs(isp)
}

// rebalanceIPs redistributes the ISP rate across per-IP limiters.
// Warmup IPs get min(warmupDailyLimit/24, equalShare).
// Active IPs split the remainder; the last active absorbs rounding.
func (r *ISPRateRegistry) rebalanceIPs(isp ISP) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ips := r.ipLists[isp]
	if len(ips) == 0 {
		return
	}
	ispRate := r.rates[isp]
	if ispRate <= 0 {
		return
	}

	equalShare := ispRate / float64(len(ips))

	// First pass: compute warmup allocations
	warmupTotal := 0.0
	var activeIPs []IPEntry
	warmupRates := make(map[string]float64)
	for _, ip := range ips {
		if ip.Status == "warmup" && ip.WarmupDailyLimit > 0 {
			hourlyFromDaily := float64(ip.WarmupDailyLimit) / 24.0
			perIP := math.Min(hourlyFromDaily, equalShare)
			warmupRates[ip.Hostname] = perIP
			warmupTotal += perIP
		} else {
			activeIPs = append(activeIPs, ip)
		}
	}

	// Second pass: distribute remainder to active IPs
	remaining := ispRate - warmupTotal
	if remaining < 0 {
		remaining = 0
	}
	numActive := len(activeIPs)
	if numActive == 0 {
		// All IPs are warmup; they get the full allocation
		for _, ip := range ips {
			r.setIPLimiter(isp, ip.Hostname, warmupRates[ip.Hostname])
		}
		return
	}

	perActive := math.Floor(remaining / float64(numActive))
	for i, ip := range activeIPs {
		ipRate := perActive
		if i == numActive-1 {
			// Last active IP absorbs rounding remainder
			ipRate = remaining - perActive*float64(numActive-1)
		}
		r.setIPLimiter(isp, ip.Hostname, ipRate)
	}
	for _, ip := range ips {
		if ip.Status == "warmup" && ip.WarmupDailyLimit > 0 {
			r.setIPLimiter(isp, ip.Hostname, warmupRates[ip.Hostname])
		}
	}

	if r.db != nil {
		go r.persistIPRates(isp)
	}
}

// setIPLimiter creates or updates a per-IP rate limiter. Caller must hold mu.
func (r *ISPRateRegistry) setIPLimiter(isp ISP, hostname string, msgsPerHour float64) {
	key := string(isp) + ":" + hostname
	perSecond := msgsPerHour / 3600.0
	burst := int(math.Max(1, msgsPerHour/360.0))

	if lim, ok := r.ipLimiters[key]; ok {
		lim.SetLimit(rate.Limit(perSecond))
		lim.SetBurst(burst)
	} else {
		r.ipLimiters[key] = rate.NewLimiter(rate.Limit(perSecond), burst)
	}
	r.ipRates[key] = msgsPerHour
}

// AllowNIP checks if a specific IP's rate limiter can permit n messages.
// Returns how many are allowed (0 to n). Unknown IPs are fully permitted.
func (r *ISPRateRegistry) AllowNIP(isp ISP, hostname string, n int) int {
	if n <= 0 {
		return 0
	}
	key := string(isp) + ":" + hostname

	r.mu.RLock()
	lim, ok := r.ipLimiters[key]
	r.mu.RUnlock()

	if !ok {
		return n
	}

	now := time.Now()
	available := int(lim.TokensAt(now))
	if available <= 0 {
		log.Printf("[rate-registry] per-IP denial: %s:%s requested=%d available=0", isp, hostname, n)
		return 0
	}

	allowed := n
	if allowed > available {
		allowed = available
	}
	if lim.AllowN(now, allowed) {
		return allowed
	}
	return 0
}

// DistributeByIP divides a requested batch count across the IPs serving an ISP,
// proportional to each IP's rate allocation, and checks per-IP token buckets.
// Returns map[hostname]allowed. If no IP list exists, returns nil (caller
// should fall back to ISP-level AllowN).
func (r *ISPRateRegistry) DistributeByIP(isp ISP, requested int) map[string]int {
	r.mu.RLock()
	ips := r.ipLists[isp]
	if len(ips) == 0 {
		r.mu.RUnlock()
		return nil
	}

	// Sum of per-IP rates for proportional distribution
	totalIPRate := 0.0
	for _, ip := range ips {
		key := string(isp) + ":" + ip.Hostname
		totalIPRate += r.ipRates[key]
	}
	r.mu.RUnlock()

	if totalIPRate <= 0 {
		return nil
	}

	result := make(map[string]int, len(ips))
	assigned := 0
	for i, ip := range ips {
		key := string(isp) + ":" + ip.Hostname
		r.mu.RLock()
		ipRate := r.ipRates[key]
		r.mu.RUnlock()

		var share int
		if i == len(ips)-1 {
			share = requested - assigned
		} else {
			share = int(math.Floor(float64(requested) * ipRate / totalIPRate))
		}
		if share <= 0 {
			continue
		}
		assigned += share

		allowed := r.AllowNIP(isp, ip.Hostname, share)
		if allowed > 0 {
			result[ip.Hostname] = allowed
		}
	}
	return result
}

// GetIPRates returns a snapshot of per-IP rates for an ISP.
func (r *ISPRateRegistry) GetIPRates(isp ISP) map[string]float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	prefix := string(isp) + ":"
	out := make(map[string]float64)
	for k, v := range r.ipRates {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out[k[len(prefix):]] = v
		}
	}
	return out
}

// HasIPList returns true if per-IP limiters are configured for this ISP.
func (r *ISPRateRegistry) HasIPList(isp ISP) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.ipLists[isp]) > 0
}

// ClearIPState removes all per-IP limiters, rates, and IP lists for an ISP.
func (r *ISPRateRegistry) ClearIPState(isp ISP) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := string(isp) + ":"
	for k := range r.ipLimiters {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(r.ipLimiters, k)
		}
	}
	for k := range r.ipRates {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(r.ipRates, k)
		}
	}
	delete(r.ipLists, isp)
}

// DistributeByIPStr is the string-typed adapter for DistributeByIP,
// matching the ISPRateLimiter interface used by the worker package.
func (r *ISPRateRegistry) DistributeByIPStr(isp string, requested int) map[string]int {
	return r.DistributeByIP(ISP(isp), requested)
}

// HasIPListStr is the string-typed adapter for HasIPList.
func (r *ISPRateRegistry) HasIPListStr(isp string) bool {
	return r.HasIPList(ISP(isp))
}

func (r *ISPRateRegistry) persistIPRates(isp ISP) {
	if r.db == nil {
		return
	}
	r.mu.RLock()
	ips := r.ipLists[isp]
	snapshot := make(map[string]float64, len(ips))
	for _, ip := range ips {
		key := string(isp) + ":" + ip.Hostname
		snapshot[ip.Hostname] = r.ipRates[key]
	}
	r.mu.RUnlock()

	ctx, cancel := r.persistCtx(5 * time.Second)
	defer cancel()
	for hostname, msgsPerHour := range snapshot {
		_, err := r.db.ExecContext(ctx, `
			INSERT INTO mailing_isp_ip_throttle_state (isp, ip_hostname, msgs_per_hour, updated_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (isp, ip_hostname) DO UPDATE SET msgs_per_hour = $3, updated_at = NOW()
		`, string(isp), hostname, msgsPerHour)
		if err != nil {
			log.Printf("[rate-registry] persist per-IP rate %s:%s failed: %v", isp, hostname, err)
		}
	}
}

// RestoreIPRatesFromDB loads persisted per-IP rates (if they exist) and
// re-populates per-IP limiters. Called alongside RestoreFromDB on startup.
func (r *ISPRateRegistry) RestoreIPRatesFromDB() int {
	if r.db == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := r.db.QueryContext(ctx, `
		SELECT isp, ip_hostname, msgs_per_hour
		FROM mailing_isp_ip_throttle_state
		WHERE updated_at > $1
	`, time.Now().Add(-throttleStateTTL))
	if err != nil {
		log.Printf("[rate-registry] restore per-IP rates failed: %v", err)
		return 0
	}
	defer rows.Close()

	restored := 0
	r.mu.Lock()
	for rows.Next() {
		var ispStr, hostname string
		var msgsPerHour float64
		if err := rows.Scan(&ispStr, &hostname, &msgsPerHour); err != nil {
			continue
		}
		isp := ISP(ispStr)
		r.setIPLimiter(isp, hostname, msgsPerHour)
		restored++
	}
	r.mu.Unlock()
	if restored > 0 {
		log.Printf("[rate-registry] restored %d per-IP rate entries from DB", restored)
	}
	return restored
}
