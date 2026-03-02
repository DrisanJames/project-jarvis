// Package smtputil provides SMTP error classification for bounce handling.
// Hard bounces (5xx) trigger immediate global suppression; soft bounces (4xx)
// are retried or tracked without suppression.
package smtputil
