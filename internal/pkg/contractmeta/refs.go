package contractmeta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/lib/pq"
)

// Queryer is the read surface the resolvers need. It is deliberately read-only
// — resolving refs never writes — and injectable so callers can pass a *sql.DB,
// a *sql.Tx, or a test double.
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ErrRefNotFound is returned when a contract points at an object that does not
// exist. §1.5 rule 3 makes this fatal: a missing sending profile or dataset
// fails the issue, rather than producing a contract with a dangling ref.
type ErrRefNotFound struct {
	Table string
	Key   string
}

func (e *ErrRefNotFound) Error() string {
	return fmt.Sprintf("contractmeta: no %s row for %q", e.Table, e.Key)
}

// RefSpec says which refs to resolve for one contract. Fields left empty are
// skipped, so a dispatch contract asks only for datasets and a domain contract
// only for its sending domain.
type RefSpec struct {
	// SendingDomain resolves both SendingDomainID (mailing_sending_profiles)
	// and OwnedDomainID (mailing_owned_domains).
	SendingDomain string
	// DatasetSlugs resolves DatasetIDs (partner_datasets.slug -> id).
	DatasetSlugs []string
	// SegmentIDs is carried through unchanged; there is no lookup for it here.
	SegmentIDs []string
}

// ResolveRefs resolves every ref named by spec, read-only. Any miss is an error
// (*ErrRefNotFound) — refs fail closed rather than resolving to an empty id.
func ResolveRefs(ctx context.Context, db Queryer, spec RefSpec) (Refs, error) {
	var out Refs

	if s := strings.TrimSpace(spec.SendingDomain); s != "" {
		profileID, err := ResolveSendingProfileID(ctx, db, s)
		if err != nil {
			return Refs{}, err
		}
		ownedID, err := ResolveOwnedDomainID(ctx, db, s)
		if err != nil {
			return Refs{}, err
		}
		out.SendingDomainID = profileID
		out.OwnedDomainID = ownedID
	}

	if len(spec.DatasetSlugs) > 0 {
		ids, err := ResolveDatasetIDs(ctx, db, spec.DatasetSlugs)
		if err != nil {
			return Refs{}, err
		}
		out.DatasetIDs = ids
	}

	if len(spec.SegmentIDs) > 0 {
		out.SegmentIDs = append([]string(nil), spec.SegmentIDs...)
		sort.Strings(out.SegmentIDs)
	}
	return out, nil
}

// ResolveSendingProfileID returns mailing_sending_profiles.id for a sending
// domain. When a domain has several profiles the default one wins, then the
// oldest — deterministic, so re-resolving the same contract cannot silently
// move the ref to a different profile.
func ResolveSendingProfileID(ctx context.Context, db Queryer, sendingDomain string) (string, error) {
	const q = `SELECT id::text FROM mailing_sending_profiles
 WHERE sending_domain = $1 AND status = 'active'
 ORDER BY is_default DESC, created_at ASC
 LIMIT 1`
	var id string
	switch err := db.QueryRowContext(ctx, q, sendingDomain).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return "", &ErrRefNotFound{Table: "mailing_sending_profiles", Key: sendingDomain}
	case err != nil:
		return "", fmt.Errorf("contractmeta: resolve sending profile for %q: %w", sendingDomain, err)
	}
	return id, nil
}

// ResolveOwnedDomainID returns the mailing_owned_domains row that owns a
// sending domain.
//
// NOTE ON THE ID: that table has no `id` column — its primary key is
// `domain TEXT` (cmd/server/main.go, create_owned_domains). The returned value
// is therefore the domain root itself, which IS the row's identity.
//
// The match is a longest-suffix match, mirroring brand.Root(): the sending
// domain `em.historythinking.com` is owned by the row `historythinking.com`.
// An exact row also matches. Longest wins so a nested root cannot shadow a
// more specific one.
func ResolveOwnedDomainID(ctx context.Context, db Queryer, sendingDomain string) (string, error) {
	const q = `SELECT domain FROM mailing_owned_domains
 WHERE active = TRUE AND ($1 = domain OR $1 LIKE '%.' || domain)
 ORDER BY length(domain) DESC
 LIMIT 1`
	var d string
	switch err := db.QueryRowContext(ctx, q, sendingDomain).Scan(&d); {
	case errors.Is(err, sql.ErrNoRows):
		return "", &ErrRefNotFound{Table: "mailing_owned_domains", Key: sendingDomain}
	case err != nil:
		return "", fmt.Errorf("contractmeta: resolve owned domain for %q: %w", sendingDomain, err)
	}
	return d, nil
}

// ResolveDatasetIDs maps partner_datasets.slug values to ids. EVERY slug must
// resolve; the first missing one is an error naming it. Ids come back sorted so
// the canonical form of a contract does not depend on the caller's slug order.
func ResolveDatasetIDs(ctx context.Context, db Queryer, slugs []string) ([]string, error) {
	want := make([]string, 0, len(slugs))
	seen := map[string]struct{}{}
	for _, s := range slugs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		want = append(want, s)
	}
	if len(want) == 0 {
		return nil, nil
	}

	rows, err := db.QueryContext(ctx,
		`SELECT slug, id::text FROM partner_datasets WHERE slug = ANY($1)`, pq.Array(want))
	if err != nil {
		return nil, fmt.Errorf("contractmeta: resolve datasets: %w", err)
	}
	defer rows.Close()

	found := map[string]string{}
	for rows.Next() {
		var slug, id string
		if err := rows.Scan(&slug, &id); err != nil {
			return nil, fmt.Errorf("contractmeta: scan dataset ref: %w", err)
		}
		found[slug] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contractmeta: iterate dataset refs: %w", err)
	}

	out := make([]string, 0, len(want))
	for _, s := range want {
		id, ok := found[s]
		if !ok {
			return nil, &ErrRefNotFound{Table: "partner_datasets", Key: s}
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}
