package api

// Partner API key authentication middleware.
//
// Validates inbound API keys against partner_api_keys (rows are keyed by a
// SHA-256 hex hash of the full key). Resolves the request to a (partner,
// dataset, vertical) tuple and stuffs that into request context for the
// downstream ingest handler. Updates last_used_at asynchronously.
//
// Key format: "dpk_<32 url-safe characters>". Prefix "dpk_" + first 4 chars
// is stored unhashed in `key_prefix` for operator UI display.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	partnerKeyHeader = "X-Partner-Key"
	partnerKeyPrefix = "dpk_"
)

type partnerCtxKey string

const (
	ctxKeyPartnerID    partnerCtxKey = "partner_ingest:partner_id"
	ctxKeyPartnerSlug  partnerCtxKey = "partner_ingest:partner_slug"
	ctxKeyPartnerName  partnerCtxKey = "partner_ingest:partner_name"
	ctxKeyDatasetID    partnerCtxKey = "partner_ingest:dataset_id"
	ctxKeyDatasetSlug  partnerCtxKey = "partner_ingest:dataset_slug"
	ctxKeyDatasetName  partnerCtxKey = "partner_ingest:dataset_name"
	ctxKeyVertical     partnerCtxKey = "partner_ingest:vertical"
	ctxKeyAPIKeyID     partnerCtxKey = "partner_ingest:api_key_id"
	ctxKeyKeyPrefix    partnerCtxKey = "partner_ingest:key_prefix"
)

// PartnerAuthContext is the resolved identity attached to a request after
// the partner-key middleware accepts the call. Every value is non-empty when
// authentication succeeds.
type PartnerAuthContext struct {
	PartnerID    string
	PartnerSlug  string
	PartnerName  string
	DatasetID    string
	DatasetSlug  string
	DatasetName  string
	Vertical     string
	APIKeyID     string
	KeyPrefix    string
}

// PartnerKeyAuth is middleware that enforces a valid partner API key on the
// request. On success it attaches a PartnerAuthContext to the request context
// and asynchronously bumps last_used_at.
func PartnerKeyAuth(db *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawKey := strings.TrimSpace(r.Header.Get(partnerKeyHeader))
			if rawKey == "" {
				partnerErrorResponse(w, http.StatusUnauthorized, "missing_api_key", "Provide your partner API key in the X-Partner-Key header")
				return
			}
			if !strings.HasPrefix(rawKey, partnerKeyPrefix) {
				partnerErrorResponse(w, http.StatusUnauthorized, "invalid_api_key_format", "API keys must start with 'dpk_'")
				return
			}

			authCtx, err := resolvePartnerKey(r.Context(), db, rawKey)
			if err != nil {
				if errors.Is(err, errPartnerKeyNotFound) || errors.Is(err, errPartnerKeyRevoked) {
					partnerErrorResponse(w, http.StatusUnauthorized, "invalid_api_key", "API key is not recognized or has been revoked")
					return
				}
				if errors.Is(err, errPartnerKeyDatasetPaused) {
					partnerErrorResponse(w, http.StatusServiceUnavailable, "dataset_paused", "Dataset is paused (emergency stop or admin pause). Contact your account manager.")
					return
				}
				partnerErrorResponse(w, http.StatusInternalServerError, "auth_lookup_failed", "Unable to validate API key right now")
				return
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxKeyPartnerID, authCtx.PartnerID)
			ctx = context.WithValue(ctx, ctxKeyPartnerSlug, authCtx.PartnerSlug)
			ctx = context.WithValue(ctx, ctxKeyPartnerName, authCtx.PartnerName)
			ctx = context.WithValue(ctx, ctxKeyDatasetID, authCtx.DatasetID)
			ctx = context.WithValue(ctx, ctxKeyDatasetSlug, authCtx.DatasetSlug)
			ctx = context.WithValue(ctx, ctxKeyDatasetName, authCtx.DatasetName)
			ctx = context.WithValue(ctx, ctxKeyVertical, authCtx.Vertical)
			ctx = context.WithValue(ctx, ctxKeyAPIKeyID, authCtx.APIKeyID)
			ctx = context.WithValue(ctx, ctxKeyKeyPrefix, authCtx.KeyPrefix)

			go bumpLastUsed(db, authCtx.APIKeyID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// PartnerAuthFromContext retrieves the resolved partner identity. Panics-free:
// returns ok=false if the middleware didn't run.
func PartnerAuthFromContext(ctx context.Context) (PartnerAuthContext, bool) {
	pid, ok := ctx.Value(ctxKeyPartnerID).(string)
	if !ok || pid == "" {
		return PartnerAuthContext{}, false
	}
	return PartnerAuthContext{
		PartnerID:    pid,
		PartnerSlug:  stringFromCtx(ctx, ctxKeyPartnerSlug),
		PartnerName:  stringFromCtx(ctx, ctxKeyPartnerName),
		DatasetID:    stringFromCtx(ctx, ctxKeyDatasetID),
		DatasetSlug:  stringFromCtx(ctx, ctxKeyDatasetSlug),
		DatasetName:  stringFromCtx(ctx, ctxKeyDatasetName),
		Vertical:     stringFromCtx(ctx, ctxKeyVertical),
		APIKeyID:     stringFromCtx(ctx, ctxKeyAPIKeyID),
		KeyPrefix:    stringFromCtx(ctx, ctxKeyKeyPrefix),
	}, true
}

func stringFromCtx(ctx context.Context, key partnerCtxKey) string {
	if v, ok := ctx.Value(key).(string); ok {
		return v
	}
	return ""
}

var (
	errPartnerKeyNotFound      = errors.New("partner key not found")
	errPartnerKeyRevoked       = errors.New("partner key revoked")
	errPartnerKeyDatasetPaused = errors.New("partner dataset paused")
)

func resolvePartnerKey(ctx context.Context, db *sql.DB, rawKey string) (PartnerAuthContext, error) {
	hash := HashPartnerKey(rawKey)

	row := db.QueryRowContext(ctx, `
		SELECT k.id, k.partner_id, k.dataset_id, COALESCE(k.key_prefix, ''),
		       p.slug, p.name,
		       d.slug, d.name, d.vertical,
		       d.paused_emergency, COALESCE(d.status, 'active'),
		       COALESCE(p.status, 'active'),
		       COALESCE(k.status, 'active')
		FROM partner_api_keys k
		JOIN data_partners p ON p.id = k.partner_id
		JOIN partner_datasets d ON d.id = k.dataset_id
		WHERE k.key_hash = $1
		LIMIT 1
	`, hash)

	var (
		auth                                                                          PartnerAuthContext
		datasetPaused                                                                 bool
		datasetStatus, partnerStatus, keyStatus                                       string
	)
	if err := row.Scan(
		&auth.APIKeyID, &auth.PartnerID, &auth.DatasetID, &auth.KeyPrefix,
		&auth.PartnerSlug, &auth.PartnerName,
		&auth.DatasetSlug, &auth.DatasetName, &auth.Vertical,
		&datasetPaused, &datasetStatus,
		&partnerStatus,
		&keyStatus,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PartnerAuthContext{}, errPartnerKeyNotFound
		}
		return PartnerAuthContext{}, fmt.Errorf("partner-key lookup: %w", err)
	}
	if keyStatus != "active" || partnerStatus != "active" {
		return PartnerAuthContext{}, errPartnerKeyRevoked
	}
	if datasetPaused || datasetStatus == "paused" || datasetStatus == "archived" {
		return PartnerAuthContext{}, errPartnerKeyDatasetPaused
	}
	return auth, nil
}

func bumpLastUsed(db *sql.DB, apiKeyID string) {
	if db == nil || apiKeyID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx, `UPDATE partner_api_keys SET last_used_at = NOW() WHERE id = $1`, apiKeyID)
}

// HashPartnerKey deterministically hashes a raw API key for storage.
// SHA-256 is sufficient because the keys themselves are 32 bytes of crypto/rand
// (no birthday-collision risk at the per-tenant scale we operate at).
func HashPartnerKey(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(sum[:])
}

// GeneratePartnerKey returns a fresh API key in the format "dpk_<32 chars>".
// Callers are responsible for showing the raw key to the operator EXACTLY ONCE
// at creation time and persisting only the hash + prefix.
func GeneratePartnerKey() (rawKey, prefix, hash string, err error) {
	const bodyBytes = 24 // 24 bytes -> 32 base64url chars
	body := make([]byte, bodyBytes)
	if _, err := rand.Read(body); err != nil {
		return "", "", "", fmt.Errorf("rand: %w", err)
	}
	encoded := strings.TrimRight(base64URLEncode(body), "=")
	rawKey = partnerKeyPrefix + encoded
	prefix = rawKey
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	hash = HashPartnerKey(rawKey)
	return rawKey, prefix, hash, nil
}

// base64URLEncode is a tiny URL-safe base64 helper without padding artefacts.
func base64URLEncode(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var sb strings.Builder
	sb.Grow((len(b)*4 + 2) / 3)
	for i := 0; i < len(b); i += 3 {
		var n uint32
		var pad int
		switch {
		case i+2 < len(b):
			n = uint32(b[i])<<16 | uint32(b[i+1])<<8 | uint32(b[i+2])
		case i+1 < len(b):
			n = uint32(b[i])<<16 | uint32(b[i+1])<<8
			pad = 1
		default:
			n = uint32(b[i]) << 16
			pad = 2
		}
		sb.WriteByte(alphabet[(n>>18)&0x3F])
		sb.WriteByte(alphabet[(n>>12)&0x3F])
		if pad < 2 {
			sb.WriteByte(alphabet[(n>>6)&0x3F])
		}
		if pad < 1 {
			sb.WriteByte(alphabet[n&0x3F])
		}
	}
	return sb.String()
}

// partnerErrorResponse writes a machine-readable JSON error.
func partnerErrorResponse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)))
}
