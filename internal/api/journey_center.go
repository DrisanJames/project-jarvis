package api

import (
	"database/sql"

	"github.com/go-chi/chi/v5"
)

// JourneyCenter handles comprehensive journey analytics and management
type JourneyCenter struct {
	db         *sql.DB
	mailingSvc *MailingService
}

// NewJourneyCenter creates a new journey center service
func NewJourneyCenter(db *sql.DB, mailingSvc *MailingService) *JourneyCenter {
	return &JourneyCenter{
		db:         db,
		mailingSvc: mailingSvc,
	}
}

// RegisterRoutes registers journey center routes under /api/mailing/journey-center
func (jc *JourneyCenter) RegisterRoutes(r chi.Router) {
	r.Route("/journey-center", func(r chi.Router) {
		// Dashboard overview
		r.Get("/overview", jc.HandleJourneyCenterOverview)

		// Journey list with filtering
		r.Get("/journeys", jc.HandleListJourneyCenterJourneys)

		// Journey-specific endpoints
		r.Route("/journeys/{journeyId}", func(r chi.Router) {
			r.Get("/metrics", jc.HandleJourneyMetrics)
			r.Get("/funnel", jc.HandleJourneyFunnel)
			r.Get("/trends", jc.HandleJourneyTrends)

			// Enrollment management
			r.Get("/enrollments", jc.HandleJourneyEnrollments)
			r.Post("/enrollments", jc.HandleManualEnrollment)
			r.Get("/enrollments/{enrollmentId}", jc.HandleEnrollmentDetail)

			// Testing
			r.Post("/test", jc.HandleTestJourney)

			// Segment enrollment
			r.Post("/segment-enroll", jc.HandleSegmentEnrollment)
		})

		// Segments for enrollment
		r.Get("/segments", jc.HandleJourneySegments)

		// Cross-journey performance
		r.Get("/performance", jc.HandleJourneyPerformanceComparison)
	})
}
