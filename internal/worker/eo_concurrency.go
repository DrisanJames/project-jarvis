package worker

import (
	"strconv"
	"strings"
)

// EOConcurrencyMax bounds the PartnerValidator's Email Oversight concurrency. Operator 2026-11-18: the daily 150,000
// EO allocation needs ~15,000 verdicts/hour sustained; 21:00 MT 11-17 measured 14,548/h at 10. The env override is
// the lever when the supply job pages "EO falling behind" — bounded so a typo can never flood the EO API.
const EOConcurrencyMax = 40

// ResolveEOConcurrency returns the effective validator concurrency and where it came from:
// PARTNER_EO_CONCURRENCY (env, 1..EOConcurrencyMax, clamped at the max) wins over cfg.DataPipeline.EOConcurrency,
// which wins over the default 10. An unparsable or non-positive env value is ignored (source says so).
func ResolveEOConcurrency(cfgVal int, envVal string) (int, string) {
	n, src := cfgVal, "config"
	if n <= 0 {
		n, src = 10, "default"
	}
	if v := strings.TrimSpace(envVal); v != "" {
		e, err := strconv.Atoi(v)
		switch {
		case err != nil || e <= 0:
			src += " (PARTNER_EO_CONCURRENCY=" + v + " ignored: not a positive integer)"
		case e > EOConcurrencyMax:
			n, src = EOConcurrencyMax, "env PARTNER_EO_CONCURRENCY="+v+" clamped to "+strconv.Itoa(EOConcurrencyMax)
		default:
			n, src = e, "env PARTNER_EO_CONCURRENCY"
		}
	}
	if n > EOConcurrencyMax {
		n, src = EOConcurrencyMax, src+" clamped to "+strconv.Itoa(EOConcurrencyMax)
	}
	return n, src
}
