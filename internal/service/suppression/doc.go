// Package suppression implements the global suppression list service.
//
// This is the single source of truth for whether an email address should
// receive mail. Suppressions flow in from multiple sources (PMTA bounces,
// FBL complaints, manual admin actions, ESP webhooks) and are checked
// before every send.
//
// The service layer contains pure business logic and depends on the
// Repository interface defined in repository.go. It never imports
// net/http or database/sql directly.
package suppression
