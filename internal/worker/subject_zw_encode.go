package worker

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// Zero-width Subject steganography — a faithful Go port of encode.py's
// encode_secret (the encoding-proj reference). It embeds an invisible secret
// payload into a visible Subject line using zero-width runes, one per bit of
// the UTF-8-encoded secret: U+200B (zero-width space) = bit 0, U+200C
// (zero-width non-joiner) = bit 1. The visible subject is byte-for-byte
// unchanged to a human reader; the hidden payload round-trips through
// encode.py's decode_secret.
//
// The mechanism is gated three ways and is OFF by default:
//   1. Global kill switch  — DISABLE_SUBJECT_ZW_ENCODE=true short-circuits.
//   2. Yahoo only          — applied solely when the RECIPIENT classifies to
//                            the "yahoo" ISP group (per-message, per-recipient).
//   3. Per-sending-domain  — the sending profile must have
//                            mailing_sending_profiles.subject_zw_encode=TRUE and
//                            a non-empty subject_zw_secret (the parameterized
//                            input). Every other profile is untouched.
const (
	zwZero = '\u200b' // zero-width space      -> binary 0
	zwOne  = '\u200c' // zero-width non-joiner -> binary 1
)

// subjectZWGroup is the single ISP class this mechanism applies to (Yahoo only).
const subjectZWGroup = isp.Yahoo

// encodeSubjectSecret embeds secret invisibly after the first rune of public,
// faithfully to encode.py encode_secret: rune[0] + <zero-width payload> +
// rune[1:]. Slicing is by rune (not byte) to match Python's codepoint slicing.
// Returns public unchanged when either input is empty — a live send must never
// break because the feature is misconfigured.
func encodeSubjectSecret(public, secret string) string {
	if public == "" || secret == "" {
		return public
	}
	var payload strings.Builder
	for _, b := range []byte(secret) { // UTF-8 bytes of the secret, MSB first
		for bit := 7; bit >= 0; bit-- {
			if (b>>uint(bit))&1 == 1 {
				payload.WriteRune(zwOne)
			} else {
				payload.WriteRune(zwZero)
			}
		}
	}
	runes := []rune(public)
	var out strings.Builder
	out.WriteRune(runes[0])
	out.WriteString(payload.String())
	if len(runes) > 1 {
		out.WriteString(string(runes[1:]))
	}
	return out.String()
}

// subjectZWConfig holds the per-profile subject-encoding facts.
type subjectZWConfig struct {
	Enabled bool
	Secret  string
}

// subjectZWDisabled reports the global kill switch.
func subjectZWDisabled() bool {
	return strings.EqualFold(os.Getenv("DISABLE_SUBJECT_ZW_ENCODE"), "true")
}

// resolveSubjectZW returns the per-profile subject-encoding config, cached for
// the process lifetime. Mirrors resolveProfileSES: a nil/empty profile id, a
// missing row, a missing column (pre-migration), or any DB error returns the
// zero value (disabled) so the default (unencoded) path is preserved.
func (p *SendWorkerPool) resolveSubjectZW(ctx context.Context, profileID string) subjectZWConfig {
	if profileID == "" {
		return subjectZWConfig{}
	}
	p.szwMu.RLock()
	if cached, ok := p.subjectZWCache[profileID]; ok {
		p.szwMu.RUnlock()
		return cached
	}
	p.szwMu.RUnlock()

	var enabled sql.NullBool
	var secret sql.NullString
	err := p.db.QueryRowContext(ctx,
		`SELECT COALESCE(subject_zw_encode, FALSE), COALESCE(subject_zw_secret, '')
		   FROM mailing_sending_profiles WHERE id = $1`,
		profileID).Scan(&enabled, &secret)
	if err != nil {
		// Pre-migration deployments hit `column "subject_zw_encode" does not
		// exist` until runStartupMigrations has run; treat as disabled instead
		// of failing the send (mirrors resolveProfileSES).
		if !strings.Contains(err.Error(), "subject_zw_encode") &&
			!strings.Contains(err.Error(), "subject_zw_secret") &&
			err != sql.ErrNoRows {
			log.Printf("resolveSubjectZW: profile=%s db error: %v", profileID, err)
		}
		return p.cacheSubjectZW(profileID, subjectZWConfig{})
	}
	cfg := subjectZWConfig{Enabled: enabled.Bool, Secret: secret.String}
	if cfg.Enabled {
		log.Printf("resolveSubjectZW: profile=%s subject_zw_encode=true", profileID)
	}
	return p.cacheSubjectZW(profileID, cfg)
}

// cacheSubjectZW stores cfg under profileID and returns it. The map is lazily
// created under the write lock so resolveSubjectZW is safe even if
// SetTrackingConfig has not yet run.
func (p *SendWorkerPool) cacheSubjectZW(profileID string, cfg subjectZWConfig) subjectZWConfig {
	p.szwMu.Lock()
	if p.subjectZWCache == nil {
		p.subjectZWCache = make(map[string]subjectZWConfig)
	}
	p.subjectZWCache[profileID] = cfg
	p.szwMu.Unlock()
	return cfg
}

// maybeEncodeSubject applies the zero-width secret to subject when all three
// gates pass: the global kill switch is off, the recipient is Yahoo, and the
// sending profile has the feature enabled with a non-empty secret. Otherwise it
// returns subject unchanged.
func (p *SendWorkerPool) maybeEncodeSubject(ctx context.Context, subject, recipientISP, profileID string) string {
	if subject == "" || subjectZWDisabled() || recipientISP != subjectZWGroup {
		return subject
	}
	cfg := p.resolveSubjectZW(ctx, profileID)
	if !cfg.Enabled || cfg.Secret == "" {
		return subject
	}
	return encodeSubjectSecret(subject, cfg.Secret)
}
