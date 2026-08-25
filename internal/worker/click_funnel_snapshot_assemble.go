package worker

// Click-funnel snapshot — ASSEMBLY. This is docs/METRIC_CONTRACT.md §10 in
// code: it is the only place the contract's rules are applied, so a screen
// cannot re-derive one of them differently.

import (
	"database/sql"
	"math"
	"sort"
	"strings"
	"time"
)

// roundPct rounds half-up to 2dp.
//
// NOT round2 (mailing_profiles.go:498), which TRUNCATES —
// float64(int(f*100))/100 — and biased every rate on the funnel screen
// downward: a 0.0186% conversion rendered as 0.01%. round2 has 24 other call
// sites and changing it is out of scope here, so click-funnel code uses this
// and leaves that alone (METRIC_CONTRACT §10.10).
func roundPct(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

// cfRate is a percentage with a guarded denominator.
func cfRate(num, den int) float64 {
	if den <= 0 {
		return 0
	}
	return roundPct(float64(num) / float64(den) * 100)
}

func nullF(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	r := roundPct(v.Float64)
	return &r
}

// assembleLane builds one lane object from the gathered parts.
func (w *ClickFunnelSnapshotWorker) assembleLane(
	l cfLaneRow,
	graph []cfGraphNode,
	maturity, lookback map[string]float64,
	flow map[cfFlowKey]cfFlow,
	awaiting map[cfFlowKey]int,
	cohort *cfCohort,
	conv *cfConversions,
	medFirstSend sql.NullFloat64,
	nodeConvs map[cfFlowKey]int,
	copyBy map[cfCopyKey]cfCopy,
	nodeCampaigns map[cfFlowKey]string,
	marks ClickFunnelWatermarks,
	snapID string,
	generatedAt time.Time,
) *ClickFunnelLane {

	if cohort == nil {
		cohort = &cfCohort{}
	}
	if conv == nil {
		conv = &cfConversions{}
	}

	lh := ladderHours(graph)
	if lh <= 0 {
		lh = clickFunnelDefaultLookbackHours
	}

	lane := &ClickFunnelLane{
		SnapshotID:    snapID,
		SchemaVersion: ClickFunnelSchemaVersion,
		GeneratedAt:   generatedAt,
		Watermarks:    marks,
		DataQuality:   cfDataQuality(marks),
		LadderHours:   roundPct(lh),
		MaturityHours: roundPct(maturity[l.OfferID]),

		TotalEnrolled:   cohort.Total,
		InFlight:        cohort.InFlight,
		ExitsBehavioral: cohort.ExitsBehavioral,
		ExitsAdmin:      cohort.ExitsAdmin,
		ExitsConverted:  cohort.ExitsConverted,

		MedianHoursEnrollToConv:    nullF(cohort.MedianEnrollToConv),
		MedianHoursFirstSendToConv: nullF(medFirstSend),
	}

	row := ClickFunnelCatalogRow{
		OfferID:        l.OfferID,
		OfferName:      l.OfferName,
		JourneyID:      l.JourneyID,
		JourneyName:    l.JourneyName,
		Enabled:        l.Enabled,
		PayoutType:     l.PayoutType,
		RoutingState:   l.RoutingState,
		RedirectOffer:  l.RedirectOffer,
		Recommendation: l.Recommendation,
		SlugInlets:     l.SlugInlets,

		ActiveNow:       cohort.Active,
		MatureEnrolled:  cohort.MatureEnrolled,
		MatureCompleted: cohort.MatureCompleted,
		// COHORT rate over MATURE enrollments with administrative exits already
		// excluded by the gather query (METRIC_CONTRACT §10.2/§10.3).
		CompletionRate: cfRate(cohort.MatureCompleted, cohort.MatureEnrolled),

		ConversionsPostEnrollment: conv.PostEnrollment,
		ConversionsPreTouch:       conv.PreTouch,
		ConversionsDripAttributed: conv.DripAttributed,
	}

	// ── nodes ───────────────────────────────────────────────────────────────
	seq := 0
	waiting := 0
	alerts := []ClickFunnelAlert{}
	nodes := make([]ClickFunnelNode, 0, len(graph))

	w.daysMu.Lock()
	for _, g := range graph {
		key := cfFlowKey{Offer: l.OfferID, Node: g.ID}
		f := flow[key]
		n := ClickFunnelNode{
			NodeID:           g.ID,
			Type:             strings.ToLower(g.Type),
			Label:            g.Name,
			Sequence:         -1,
			DelayMs:          g.delayMillis(),
			Reached:          f.Reached,
			Awaiting:         awaiting[key],
			ErrorEnrollments: f.ErrorEnrollments,
			ErrorAttempts:    f.ErrorAttempts,
		}
		if n.Label == "" {
			n.Label = g.ID
		}
		waiting += n.Awaiting

		if n.Type == "email" {
			if g.Config.ReminderIndex != nil {
				n.Sequence = *g.Config.ReminderIndex
			} else {
				n.Sequence = seq
			}
			seq++

			row.TouchesEnabled++
			if c, ok := copyBy[cfCopyKey{Offer: l.OfferID, Seq: n.Sequence}]; ok {
				n.Subject, n.Preheader, n.FromOverride = c.Subject, c.Preheader, c.FromOverride
				n.CopyEnabled = c.Enabled
				n.ProofID, n.ProofName = c.ProofID, c.ProofName
				n.ProofApproval, n.ProofActive = c.ProofApproval, c.ProofActive
				// Mirrors the SENDER's gate exactly (journey_executor.go): a
				// proof that is not approved AND active is refused there and
				// the touch falls through to the snapshot, then to the clicked
				// campaign's creative.
				n.ProofSendable = c.ProofActive && strings.EqualFold(c.ProofApproval, "approved")
				n.BodyInherited = c.ProofID == "" && !c.HasBodySnapshot
				if !c.UpdatedAt.IsZero() {
					n.CopyUpdatedAt = c.UpdatedAt.UTC().Format(time.RFC3339)
				}
				if c.ProofID != "" {
					row.TouchesWithProof++
				}
				if n.ProofSendable {
					row.TouchesSendable++
				}
			} else {
				n.CopyMissing = true
				n.BodyInherited = true
			}

			n.Conversions = nodeConvs[key]
			n.ConversionLookbackHours = roundPct(lookback[l.OfferID])

			if cid, ok := nodeCampaigns[key]; ok && cid != "" {
				n.ShadowCampaignID = cid
				n.Attributed = true
				if byDt := w.days[key]; byDt != nil {
					days := make([]ClickFunnelNodeDay, 0, len(byDt))
					for _, d := range byDt {
						days = append(days, d)
					}
					sort.Slice(days, func(i, j int) bool { return days[i].Dt < days[j].Dt })
					n.Days = days
				}
			}

			// ── per-node alerts ────────────────────────────────────────────
			// A stuck retry is an OPERATIONAL condition, not a metric: three
			// mailboxes retrying every two minutes for 13 days produced 26,908
			// attempts and read as 26,904 broken recipients.
			if n.ErrorEnrollments > 0 && n.ErrorAttempts/max1(n.ErrorEnrollments) >= clickFunnelStuckRetryRatio {
				alerts = append(alerts, ClickFunnelAlert{
					Code: ClickFunnelAlertStuckRetry, Severity: "critical", NodeID: g.ID,
					Count: n.ErrorEnrollments,
					Message: "stuck retry: " + cfItoa(n.ErrorEnrollments) + " enrollment(s) generated " +
						cfItoa(n.ErrorAttempts) + " attempts (" + cfItoa(n.ErrorAttempts/max1(n.ErrorEnrollments)) + "x)",
				})
			}
			if !n.Attributed {
				alerts = append(alerts, ClickFunnelAlert{
					Code: ClickFunnelAlertUnattributed, Severity: "warning", NodeID: g.ID,
					Message: "no shadow campaign — this touch cannot be measured; its engagement is invisible",
				})
			}
			if n.CopyMissing {
				alerts = append(alerts, ClickFunnelAlert{
					Code: ClickFunnelAlertCopyMissing, Severity: "warning", NodeID: g.ID,
					Message: "no copy row — the touch inherits the clicked campaign's subject",
				})
			} else if n.ProofID == "" {
				alerts = append(alerts, ClickFunnelAlert{
					Code: ClickFunnelAlertNoProof, Severity: "warning", NodeID: g.ID,
					Message: "no Creative Studio proof — mails whatever creative the subscriber clicked",
				})
			} else if !n.ProofSendable {
				alerts = append(alerts, ClickFunnelAlert{
					Code: ClickFunnelAlertProofUnsendable, Severity: "critical", NodeID: g.ID,
					Message: "proof is " + orDash(n.ProofApproval) + "/" + activeWord(n.ProofActive) +
						" — the sender REFUSES it and falls through to inherited creative",
				})
			}
		}

		if strings.EqualFold(g.Type, "goal") {
			lane.GoalNodeReached = f.Reached
		}
		nodes = append(nodes, n)
	}
	w.daysMu.Unlock()

	row.WaitingNow = waiting

	// ── lane-level alerts ───────────────────────────────────────────────────
	if l.Enabled && l.SlugInlets == 0 {
		alerts = append(alerts, ClickFunnelAlert{
			Code: ClickFunnelAlertNoInlet, Severity: "warning",
			Message: "lane is enabled but has no money-slug inlet — it can never enroll",
		})
	}
	if cohort.Total > 0 {
		if pct := float64(cohort.ExitsAdmin) / float64(cohort.Total) * 100; pct >= clickFunnelAdminExitAlertPct {
			alerts = append(alerts, ClickFunnelAlert{
				Code: ClickFunnelAlertAdminExits, Severity: "info", Count: cohort.ExitsAdmin,
				Message: cfItoa(cohort.ExitsAdmin) + " enrollments (" + cfFtoa(roundPct(pct)) +
					"%) left via operator bulk action, not lane behaviour — excluded from every rate here",
			})
		}
	}

	sort.SliceStable(alerts, func(i, j int) bool {
		return cfSeverityRank(alerts[i].Severity) < cfSeverityRank(alerts[j].Severity)
	})

	row.AlertCount = len(alerts)
	row.Alerts = make([]string, 0, len(alerts))
	for _, a := range alerts {
		row.Alerts = append(row.Alerts, a.Severity+":"+a.Code)
	}

	lane.ClickFunnelCatalogRow = row
	lane.Nodes = nodes
	lane.Alerts = alerts
	lane.Notes = cfLaneNotes(lane, marks)
	return lane
}

// cfLaneNotes are the disclosures a reader needs to not misread the payload.
// They travel WITH the data so a screen cannot drop them.
func cfLaneNotes(l *ClickFunnelLane, m ClickFunnelWatermarks) []string {
	notes := []string{
		"Completion is a COHORT rate over MATURE enrollments only (enrolled at least " +
			cfFtoa(l.MaturityHours) + "h ago = ladder + 24h grace). In-flight enrollments are reported separately and are in no rate.",
		"Administrative exits (operator bulk actions) are excluded from every denominator and reported on their own line.",
		"Conversions are three figures: post-enrollment, pre-touch (credit belongs to the original click) and drip-attributed. Only drip-attributed may be divided by a touch.",
		"Rate base is ACCEPTED (delivered + relayed_to_ses), not delivered — click-drip mail is handed to SES and books relayed_to_ses. Accepted is not inbox placement.",
		"Clicks are reported raw / classified / qualified with coverage. is_machine_click is INERT in production, so a 'human click' figure derived from it would equal the raw click.",
		m.LakeLagNote,
	}
	if l.ExitsConverted > 0 {
		notes = append(notes, "Converted enrollments carry status='exited' — conversions are a SUBSET of exits, not a fourth disjoint bucket.")
	}
	if l.GoalNodeReached != l.MatureCompleted {
		notes = append(notes, "goal_node_reached ("+cfItoa(l.GoalNodeReached)+
			") is a flow diagnostic from the execution log and differs from the canonical enrollment-status completion by design.")
	}
	if m.LakeError != "" {
		notes = append(notes, "ENGAGEMENT IS STALE: the last lake pass failed ("+m.LakeError+"). Numbers shown are the last good ones, not zero.")
	}
	return notes
}

func cfSeverityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unset"
	}
	return s
}

func activeWord(b bool) string {
	if b {
		return "active"
	}
	return "inactive"
}
