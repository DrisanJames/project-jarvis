// Package guard provides fail-closed environment guardrails for DB-touching
// admin tools, protecting against the two-environment hazard: the production
// database lives in us-west-2, but a SEPARATE us-east-1 environment has a
// DIFFERENT database. Pointing a mutating tool at the wrong DATABASE_URL
// corrupts the wrong system. This mirrors the deploy/_guardrails.sh shell guard
// for the Go tools that connect via DATABASE_URL rather than the AWS CLI.
package guard

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// localDBHosts are treated as local development databases — safe to run against
// without confirmation.
var localDBHosts = map[string]bool{
	"":                     true,
	"localhost":            true,
	"127.0.0.1":            true,
	"::1":                  true,
	"apex-postgres":        true,
	"host.docker.internal": true,
}

// dbHost extracts the host from a postgres DSN (URL form preferred, key=value fallback).
func dbHost(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.Host != "" {
		return u.Hostname()
	}
	for _, field := range strings.Fields(dsn) {
		if strings.HasPrefix(field, "host=") {
			return strings.TrimPrefix(field, "host=")
		}
	}
	return ""
}

// isRemote reports whether the DSN targets a non-local (prod-like) database.
func isRemote(dsn string) bool {
	return !localDBHosts[dbHost(dsn)]
}

// decision is the pure, testable core. Returns (allowed, message). Mutating
// tools refuse a remote DB unless confirmed; read-only tools always proceed but
// announce the target for visibility; local databases proceed freely.
func decision(dsn, tool string, mutating, confirmed bool) (bool, string) {
	host := dbHost(dsn)
	if !isRemote(dsn) {
		return true, fmt.Sprintf("[guard] %s → local dev database (host=%q)", tool, host)
	}
	if !mutating {
		return true, fmt.Sprintf("[guard] %s → REMOTE database (host=%q) — read-only, proceeding", tool, host)
	}
	if confirmed {
		return true, fmt.Sprintf("[guard] %s → REMOTE database (host=%q) — confirmed via IGNITE_DB_CONFIRM=1", tool, host)
	}
	return false, fmt.Sprintf(
		"[guard] REFUSING: %s would run against REMOTE database host=%q.\n"+
			"  This may be the WRONG environment (the us-east-1 DB is a DIFFERENT database).\n"+
			"  Set IGNITE_DB_CONFIRM=1 to confirm you intend to target this database.", tool, host)
}

// RequireDBConfirmation guards a DB-touching tool. For mutating tools it exits
// (code 87) when targeting a remote database without IGNITE_DB_CONFIRM=1.
// Read-only tools (mutating=false) always proceed but announce the target.
func RequireDBConfirmation(dsn, tool string, mutating bool) {
	confirmed := os.Getenv("IGNITE_DB_CONFIRM") == "1"
	ok, msg := decision(dsn, tool, mutating, confirmed)
	fmt.Fprintln(os.Stderr, msg)
	if !ok {
		os.Exit(87)
	}
}
