package everflow

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"
)

// volumeResolver encapsulates per-data-set-code volume resolution for data
// partner analytics. It is initialised once per buildDataPartnerAnalytics call.
type volumeResolver struct {
	volumeByDS             map[string]int64
	totalESPSends          int64
	hasMatchingVolume      bool
	grandTotalClicks       int64
	grandTotalConversions  int64
	useClicksForAllocation bool
}

// newVolumeResolver fetches volume data and initialises the resolver.
// knownPrefixes is the set of partner group prefixes already accumulated.
func (c *Collector) newVolumeResolver(
	knownPrefixes map[string]bool,
	grandTotalClicks, grandTotalConversions int64,
	startDate, endDate time.Time,
) *volumeResolver {
	vr := &volumeResolver{
		grandTotalClicks:       grandTotalClicks,
		grandTotalConversions:  grandTotalConversions,
		useClicksForAllocation: grandTotalClicks > 0,
	}

	// Fetch volume data
	if c.volumeProviderForDateRange != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		vr.volumeByDS = c.volumeProviderForDateRange(ctx, startDate, endDate)
		cancel()
		if len(vr.volumeByDS) > 0 {
			log.Printf("DataPartner: Using date-filtered volume provider for %s to %s (%d entries)",
				startDate.Format("2006-01-02"), endDate.Format("2006-01-02"), len(vr.volumeByDS))
		}
	}
	if len(vr.volumeByDS) == 0 && c.volumeProvider != nil {
		vr.volumeByDS = c.volumeProvider()
	}

	// Check for matching volume keys
	if len(vr.volumeByDS) > 2 {
		for key := range vr.volumeByDS {
			if !isValidVolumeKey(key) {
				continue
			}
			groupKey, _ := ResolvePartnerGroup(key)
			if knownPrefixes[groupKey] {
				vr.hasMatchingVolume = true
				break
			}
		}
	}

	// Resolve totalESPSends from multiple sources
	if c.totalSendsForDateRange != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		vr.totalESPSends = c.totalSendsForDateRange(ctx, startDate, endDate)
		cancel()
	}
	if vr.totalESPSends == 0 && c.metrics != nil && c.metrics.ESPRevenue != nil {
		for _, esp := range c.metrics.ESPRevenue {
			vr.totalESPSends += esp.TotalSent
		}
	}
	if vr.totalESPSends == 0 && len(vr.volumeByDS) > 0 {
		for _, v := range vr.volumeByDS {
			vr.totalESPSends += v
		}
	}
	log.Printf("DataPartner: totalESPSends = %d for date range %s to %s",
		vr.totalESPSends, startDate.Format("2006-01-02"), endDate.Format("2006-01-02"))

	return vr
}

func isValidVolumeKey(key string) bool {
	if strings.Contains(key, "{{") || strings.Contains(key, "}}") {
		return false
	}
	upper := strings.ToUpper(key)
	return upper != "N/A" && upper != "NA" && upper != "WMRY" && upper != "NULL" && upper != "TESTDATASET"
}

func (vr *volumeResolver) proportionalVolume(clicks, conversions int64) int64 {
	if vr.totalESPSends > 0 {
		if vr.useClicksForAllocation && vr.grandTotalClicks > 0 {
			return int64(float64(clicks) / float64(vr.grandTotalClicks) * float64(vr.totalESPSends))
		} else if vr.grandTotalConversions > 0 {
			return int64(float64(conversions) / float64(vr.grandTotalConversions) * float64(vr.totalESPSends))
		}
	}
	return 0
}

func (vr *volumeResolver) resolveVolume(dsCode string, clicks, conversions int64) int64 {
	if vr.hasMatchingVolume {
		if v := vr.volumeByDS[strings.ToUpper(dsCode)]; v > 0 {
			return v
		}
	}
	return vr.proportionalVolume(clicks, conversions)
}

func (vr *volumeResolver) resolvePartnerVolume(prefix string, clicks, conversions int64) int64 {
	if vr.hasMatchingVolume {
		var total int64
		for key, vol := range vr.volumeByDS {
			if !isValidVolumeKey(key) {
				continue
			}
			groupKey, _ := ResolvePartnerGroup(key)
			if groupKey == prefix {
				total += vol
			}
		}
		if total > 0 {
			return total
		}
	}
	return vr.proportionalVolume(clicks, conversions)
}

// sortOfferBreakdown sorts offer-partner metrics by revenue descending (insertion sort).
func sortOfferBreakdown(s []OfferPartnerMetrics) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Revenue > s[j-1].Revenue; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// sortDataSetBreakdown sorts data-set-code metrics by revenue descending (insertion sort).
func sortDataSetBreakdown(s []DataSetCodeMetrics) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Revenue > s[j-1].Revenue; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// sortDailyMetrics sorts daily metrics by date ascending (insertion sort).
func sortDailyMetrics(s []DataPartnerDailyMetrics) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Date < s[j-1].Date; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// sortPartnersByRevenue sorts data-partner performance by revenue descending (insertion sort).
func sortPartnersByRevenue(s []DataPartnerPerformance) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Revenue > s[j-1].Revenue; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// buildOfferCentricView inverts the partner→offer breakdown into an offer→partners
// view, returning separate CPM and CPA offer slices sorted by total revenue.
func buildOfferCentricView(partners []DataPartnerPerformance) (cpmOffers, cpaOffers []OfferWithPartnerBreakdown) {
	type offerCentricAccum struct {
		offerID   string
		offerName string
		isCPM     bool
		partners  map[string]*OfferPartnerBreakdownEntry // keyed by partner prefix
	}
	offerCentricMap := make(map[string]*offerCentricAccum) // keyed by offer ID

	for _, p := range partners {
		for _, o := range p.OfferBreakdown {
			oc, ok := offerCentricMap[o.OfferID]
			if !ok {
				oc = &offerCentricAccum{
					offerID:   o.OfferID,
					offerName: o.OfferName,
					isCPM:     o.IsCPM,
					partners:  make(map[string]*OfferPartnerBreakdownEntry),
				}
				offerCentricMap[o.OfferID] = oc
			}
			pe, ok := oc.partners[p.PartnerPrefix]
			if !ok {
				pe = &OfferPartnerBreakdownEntry{
					PartnerPrefix: p.PartnerPrefix,
					PartnerName:   p.PartnerName,
				}
				oc.partners[p.PartnerPrefix] = pe
			}
			pe.Clicks += o.Clicks
			pe.Conversions += o.Conversions
			pe.Revenue += o.Revenue
		}
	}

	for _, oc := range offerCentricMap {
		owpb := OfferWithPartnerBreakdown{
			OfferID:   oc.offerID,
			OfferName: oc.offerName,
			IsCPM:     oc.isCPM,
		}
		for _, pe := range oc.partners {
			owpb.TotalClicks += pe.Clicks
			owpb.TotalConv += pe.Conversions
			owpb.TotalRevenue += pe.Revenue
			owpb.Partners = append(owpb.Partners, *pe)
		}
		// Compute click share percentages
		for i := range owpb.Partners {
			if owpb.TotalClicks > 0 {
				owpb.Partners[i].ClickShare = float64(owpb.Partners[i].Clicks) / float64(owpb.TotalClicks) * 100
			}
		}
		// Sort partners by revenue descending
		sort.Slice(owpb.Partners, func(i, j int) bool {
			return owpb.Partners[i].Revenue > owpb.Partners[j].Revenue
		})
		if oc.isCPM {
			cpmOffers = append(cpmOffers, owpb)
		} else {
			cpaOffers = append(cpaOffers, owpb)
		}
	}
	// Sort offers by total revenue descending
	sort.Slice(cpmOffers, func(i, j int) bool {
		return cpmOffers[i].TotalRevenue > cpmOffers[j].TotalRevenue
	})
	sort.Slice(cpaOffers, func(i, j int) bool {
		return cpaOffers[i].TotalRevenue > cpaOffers[j].TotalRevenue
	})
	return
}

// buildDataPartnerMoM computes month-over-month comparison metrics for data partner
// conversions. It uses allConversions (the lookback cache) for time-series attribution.
func (c *Collector) buildDataPartnerMoM(totalClicks int64) DataPartnerMoMComparison {
	now := time.Now()
	curMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	prevMonthStart := curMonthStart.AddDate(0, -1, 0)
	prevMonthEnd := curMonthStart.Add(-time.Nanosecond)

	var curConv, prevConv int64
	var curRev, prevRev float64

	for _, conv := range c.allConversions {
		if conv.DataPartner == "" {
			continue
		}
		ct := conv.ConversionTime
		if !ct.Before(curMonthStart) && !ct.After(now) {
			curConv++
			curRev += conv.Revenue
		} else if !ct.Before(prevMonthStart) && !ct.After(prevMonthEnd) {
			prevConv++
			prevRev += conv.Revenue
		}
	}

	// For MoM click comparison we use the total clicks from the entity report
	// (the entity report covers the full lookback, not split by month).
	// We attribute all clicks to current month as an approximation.
	var curClicks, prevClicks int64
	curClicks = totalClicks // best available approximation

	mom := DataPartnerMoMComparison{
		CurrentMonth: DataPartnerPeriodSummary{
			Label:       curMonthStart.Format("January 2006"),
			Clicks:      curClicks,
			Conversions: curConv,
			Revenue:     curRev,
		},
		PreviousMonth: DataPartnerPeriodSummary{
			Label:       prevMonthStart.Format("January 2006"),
			Clicks:      prevClicks,
			Conversions: prevConv,
			Revenue:     prevRev,
		},
	}
	if prevRev > 0 {
		mom.RevenueChangePct = (curRev - prevRev) / prevRev * 100
	}
	if prevConv > 0 {
		mom.ConversionsChangePct = (float64(curConv) - float64(prevConv)) / float64(prevConv) * 100
	}
	if prevClicks > 0 {
		mom.ClicksChangePct = (float64(curClicks) - float64(prevClicks)) / float64(prevClicks) * 100
	}

	return mom
}
