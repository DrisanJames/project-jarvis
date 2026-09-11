package contentdesk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Store is the only SQL surface of the content desk. Every method is
// org-scoped; every multi-statement mutation is one transaction.
type Store struct {
	db *sql.DB
}

// NewStore wraps db.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

type querier interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

type scanner interface{ Scan(dest ...any) error }

func jsonOrDefault(b json.RawMessage, def string) []byte {
	if len(b) == 0 {
		return []byte(def)
	}
	return b
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil || b == nil {
		return []byte("null")
	}
	return b
}

func isUniqueViolation(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23505"
}

func validUUID(ids ...string) error {
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("%w: %q is not a uuid", ErrInvalid, id)
		}
	}
	return nil
}

func rollback(tx *sql.Tx) { _ = tx.Rollback() } // no-op after Commit

// ── Sites ───────────────────────────────────────────────────────────────

// Site is one brand property.
type Site struct {
	ID         string          `json:"id"`
	BrandCode  string          `json:"brand_code"`
	Domain     string          `json:"domain"`
	Surface    string          `json:"surface"`
	Enabled    bool            `json:"enabled"`
	Voice      json.RawMessage `json:"voice"`
	Categories json.RawMessage `json:"categories"`
	Adapter    json.RawMessage `json:"adapter"`
}

const siteCols = `id, brand_code, domain, surface, enabled, voice, categories, adapter`

func scanSite(sc scanner) (Site, error) {
	var st Site
	var voice, cats, adapter []byte
	if err := sc.Scan(&st.ID, &st.BrandCode, &st.Domain, &st.Surface, &st.Enabled, &voice, &cats, &adapter); err != nil {
		return st, err
	}
	st.Voice, st.Categories, st.Adapter = voice, cats, adapter
	return st, nil
}

// ListSites returns the org's sites.
func (s *Store) ListSites(ctx context.Context, org string) ([]Site, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+siteCols+` FROM content_sites WHERE org_id = $1 ORDER BY brand_code`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Site{}
	for rows.Next() {
		st, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) getSite(ctx context.Context, q querier, org, id string) (Site, error) {
	st, err := scanSite(q.QueryRowContext(ctx, `SELECT `+siteCols+` FROM content_sites WHERE id = $1 AND org_id = $2`, id, org))
	if errors.Is(err, sql.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

// SitePatch is the PATCH sites/{id} body; nil fields are untouched.
type SitePatch struct {
	Enabled    *bool           `json:"enabled"`
	Voice      json.RawMessage `json:"voice"`
	Categories json.RawMessage `json:"categories"`
	Adapter    json.RawMessage `json:"adapter"`
}

// PatchSite updates the editable site fields.
func (s *Store) PatchSite(ctx context.Context, org, id string, p SitePatch) (Site, error) {
	if err := validUUID(id); err != nil {
		return Site{}, err
	}
	if p.Enabled == nil && len(p.Voice) == 0 && len(p.Categories) == 0 && len(p.Adapter) == 0 {
		return Site{}, fmt.Errorf("%w: nothing to update", ErrInvalid)
	}
	args := []any{id, org, nil, nil, nil, nil}
	if p.Enabled != nil {
		args[2] = *p.Enabled
	}
	for i, raw := range []json.RawMessage{p.Voice, p.Categories, p.Adapter} {
		if len(raw) == 0 {
			continue
		}
		if !json.Valid(raw) {
			return Site{}, fmt.Errorf("%w: invalid JSON", ErrInvalid)
		}
		args[3+i] = []byte(raw)
	}
	st, err := scanSite(s.db.QueryRowContext(ctx, `
		UPDATE content_sites SET
			enabled    = COALESCE($3::boolean, enabled),
			voice      = COALESCE($4::jsonb, voice),
			categories = COALESCE($5::jsonb, categories),
			adapter    = COALESCE($6::jsonb, adapter),
			updated_at = NOW()
		WHERE id = $1 AND org_id = $2
		RETURNING `+siteCols, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

// ── Briefs ──────────────────────────────────────────────────────────────

// BriefInput is the POST briefs body.
type BriefInput struct {
	SiteID         string          `json:"site_id"`
	Format         string          `json:"format"`
	ReaderQuestion string          `json:"reader_question"`
	Angle          string          `json:"angle"`
	Outline        json.RawMessage `json:"outline"`
	Category       string          `json:"category"`
	Consequential  bool            `json:"consequential"`
	CreatedBy      string          `json:"created_by"`
	Slug           string          `json:"slug"`
}

// Brief is one content_briefs row (+ its article id).
type Brief struct {
	ID             string          `json:"id"`
	SiteID         string          `json:"site_id"`
	Format         string          `json:"format"`
	ReaderQuestion string          `json:"reader_question"`
	Angle          string          `json:"angle"`
	Outline        json.RawMessage `json:"outline"`
	Category       string          `json:"category"`
	Consequential  bool            `json:"consequential"`
	Status         string          `json:"status"`
	CreatedBy      string          `json:"created_by"`
	CreatedAt      time.Time       `json:"created_at"`
	ArticleID      string          `json:"article_id,omitempty"`
}

// CreateBrief inserts a brief and its drafting article in one transaction.
// consequential is forced on for ConsequentialCategories — the client cannot
// turn it off.
func (s *Store) CreateBrief(ctx context.Context, org string, in BriefInput) (Brief, error) {
	if err := validUUID(in.SiteID); err != nil {
		return Brief{}, err
	}
	in.Format, in.ReaderQuestion = strings.TrimSpace(in.Format), strings.TrimSpace(in.ReaderQuestion)
	if in.Format == "" || in.ReaderQuestion == "" {
		return Brief{}, fmt.Errorf("%w: format and reader_question are required", ErrInvalid)
	}
	if !ValidCategory(in.Category) {
		return Brief{}, fmt.Errorf("%w: category must be one of finance|tax|health|benefits|insurance|history|diy", ErrInvalid)
	}
	outline := jsonOrDefault(in.Outline, "[]")
	if !json.Valid(outline) {
		return Brief{}, fmt.Errorf("%w: outline is not JSON", ErrInvalid)
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = Slugify(in.ReaderQuestion)
	}
	if !slugRE.MatchString(slug) || len(slug) > MaxSlugLen {
		return Brief{}, fmt.Errorf("%w: slug %q must be lowercase-hyphenated, ≤%d chars", ErrInvalid, slug, MaxSlugLen)
	}
	b := Brief{SiteID: in.SiteID, Format: in.Format, ReaderQuestion: in.ReaderQuestion, Angle: in.Angle,
		Outline: outline, Category: in.Category, Consequential: in.Consequential || ConsequentialCategories[in.Category],
		Status: "open", CreatedBy: in.CreatedBy}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return b, err
	}
	defer rollback(tx)
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT enabled FROM content_sites WHERE id = $1 AND org_id = $2`, in.SiteID, org).Scan(&enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return b, ErrNotFound
		}
		return b, err
	}
	if !enabled {
		return b, fmt.Errorf("%w: site is disabled", ErrInvalid)
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO content_briefs (org_id, site_id, format, reader_question, angle, outline, category, consequential, status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, 'open', $9)
		RETURNING id, created_at`,
		org, in.SiteID, b.Format, b.ReaderQuestion, b.Angle, outline, b.Category, b.Consequential, b.CreatedBy,
	).Scan(&b.ID, &b.CreatedAt); err != nil {
		return b, err
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO content_articles (org_id, site_id, brief_id, slug, status)
		VALUES ($1, $2, $3, $4, 'drafting')
		RETURNING id`, org, in.SiteID, b.ID, slug).Scan(&b.ArticleID); err != nil {
		if isUniqueViolation(err) {
			return b, fmt.Errorf("%w: slug %q already exists on this site", ErrConflict, slug)
		}
		return b, err
	}
	return b, tx.Commit()
}

const briefSelect = `SELECT b.id, b.site_id, b.format, b.reader_question, b.angle, b.outline, b.category,
	b.consequential, b.status, b.created_by, b.created_at, COALESCE(a.id::text, '')
	FROM content_briefs b LEFT JOIN content_articles a ON a.brief_id = b.id`

func scanBrief(sc scanner) (Brief, error) {
	var b Brief
	var outline []byte
	err := sc.Scan(&b.ID, &b.SiteID, &b.Format, &b.ReaderQuestion, &b.Angle, &outline, &b.Category,
		&b.Consequential, &b.Status, &b.CreatedBy, &b.CreatedAt, &b.ArticleID)
	b.Outline = outline
	return b, err
}

// ListBriefs lists briefs, optionally for one site.
func (s *Store) ListBriefs(ctx context.Context, org, siteID string) ([]Brief, error) {
	rows, err := s.db.QueryContext(ctx, briefSelect+`
		WHERE b.org_id = $1 AND ($2 = '' OR b.site_id::text = $2)
		ORDER BY b.created_at DESC LIMIT 500`, org, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Brief{}
	for rows.Next() {
		b, err := scanBrief(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) getBrief(ctx context.Context, q querier, org, id string) (Brief, error) {
	b, err := scanBrief(q.QueryRowContext(ctx, briefSelect+` WHERE b.id = $1 AND b.org_id = $2`, id, org))
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

// ── Articles ────────────────────────────────────────────────────────────

// Article is one content_articles row with its current revision's hash/title.
type Article struct {
	ID                string    `json:"id"`
	SiteID            string    `json:"site_id"`
	BriefID           string    `json:"brief_id"`
	Slug              string    `json:"slug"`
	Status            string    `json:"status"`
	CurrentRevisionID string    `json:"current_revision_id"`
	RevisionHash      string    `json:"revision_hash"`
	Title             string    `json:"title"`
	Harvestable       bool      `json:"harvestable"`
	RunRequested      bool      `json:"run_requested"`
	UpdatedAt         time.Time `json:"updated_at"`
}

const articleSelect = `SELECT a.id, a.site_id, COALESCE(a.brief_id::text, ''), a.slug, a.status,
	COALESCE(a.current_revision_id::text, ''), COALESCE(r.revision_hash, ''), COALESCE(r.package->>'title', ''),
	a.harvestable, a.run_requested_at IS NOT NULL, a.updated_at
	FROM content_articles a LEFT JOIN content_revisions r ON r.id = a.current_revision_id`

func scanArticle(sc scanner) (Article, error) {
	var a Article
	err := sc.Scan(&a.ID, &a.SiteID, &a.BriefID, &a.Slug, &a.Status, &a.CurrentRevisionID, &a.RevisionHash,
		&a.Title, &a.Harvestable, &a.RunRequested, &a.UpdatedAt)
	return a, err
}

// ListArticles filters by status and site.
func (s *Store) ListArticles(ctx context.Context, org, status, siteID string) ([]Article, error) {
	rows, err := s.db.QueryContext(ctx, articleSelect+`
		WHERE a.org_id = $1 AND ($2 = '' OR a.status = $2) AND ($3 = '' OR a.site_id::text = $3)
		ORDER BY a.updated_at DESC LIMIT 500`, org, status, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Article{}
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) getArticle(ctx context.Context, q querier, org, id string) (Article, error) {
	a, err := scanArticle(q.QueryRowContext(ctx, articleSelect+` WHERE a.id = $1 AND a.org_id = $2`, id, org))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// EnqueueRun queues the pipeline for an article in drafting or
// changes_requested. A human re-enqueue also restores the retry budget of
// failed stage runs.
func (s *Store) EnqueueRun(ctx context.Context, org, id string) error {
	if err := validUUID(id); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var got string
	err = tx.QueryRowContext(ctx, `
		UPDATE content_articles SET status = 'drafting', run_requested_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND org_id = $2 AND status IN ('drafting', 'changes_requested')
		RETURNING id`, id, org).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		var status string
		if e := tx.QueryRowContext(ctx, `SELECT status FROM content_articles WHERE id = $1 AND org_id = $2`, id, org).Scan(&status); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return ErrNotFound
			}
			return e
		}
		return fmt.Errorf("%w: an article in status %q cannot be run", ErrConflict, status)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE content_pipeline_runs SET attempts = 0 WHERE article_id = $1 AND status = 'failed'`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// StopRun dequeues an article (its stage runs exhausted or refused).
func (s *Store) StopRun(ctx context.Context, org, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE content_articles SET run_requested_at = NULL, updated_at = NOW() WHERE id = $1 AND org_id = $2`, id, org)
	return err
}

// Revision is one content_revisions row.
type Revision struct {
	ID           string     `json:"id"`
	ArticleID    string     `json:"article_id"`
	RevisionHash string     `json:"revision_hash"`
	Package      Package    `json:"package"`
	ClaimRefs    []ClaimRef `json:"claim_refs"`
	Checks       Checks     `json:"checks"`
	Usage        Usage      `json:"usage"`
	CreatedAt    time.Time  `json:"created_at"`
}

func (s *Store) getRevision(ctx context.Context, q querier, org, id string) (*Revision, error) {
	var r Revision
	var pkg, refs, checks, usage []byte
	err := q.QueryRowContext(ctx, `
		SELECT id, article_id, revision_hash, package, claim_refs, checks, usage, created_at
		FROM content_revisions WHERE id = $1 AND org_id = $2`, id, org).
		Scan(&r.ID, &r.ArticleID, &r.RevisionHash, &pkg, &refs, &checks, &usage, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	for _, x := range []struct {
		b []byte
		v any
	}{{pkg, &r.Package}, {refs, &r.ClaimRefs}, {checks, &r.Checks}, {usage, &r.Usage}} {
		if len(x.b) > 0 {
			if err := json.Unmarshal(x.b, x.v); err != nil {
				return nil, fmt.Errorf("revision %s: %w", id, err)
			}
		}
	}
	return &r, nil
}

// Review is one content_reviews row.
type Review struct {
	ID           string    `json:"id"`
	RevisionID   string    `json:"revision_id"`
	RevisionHash string    `json:"revision_hash"`
	Reviewer     string    `json:"reviewer"`
	Role         string    `json:"role"`
	Decision     string    `json:"decision"`
	Findings     []Finding `json:"findings"`
	AcceptedIDs  []string  `json:"accepted_ids"`
	Minutes      float64   `json:"minutes"`
	CreatedAt    time.Time `json:"created_at"`
}

// RunSummary is the latest run per stage for an article.
type RunSummary struct {
	Stage      string     `json:"stage"`
	Status     string     `json:"status"`
	Attempts   int        `json:"attempts"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// ArticleDetail is everything the review UI needs.
type ArticleDetail struct {
	Article  Article        `json:"article"`
	Brief    Brief          `json:"brief"`
	Site     Site           `json:"site"`
	Revision *Revision      `json:"revision"`
	Claims   []Claim        `json:"claims"`
	Reviews  []Review       `json:"reviews"`
	Pairs    []SentencePair `json:"pairs"`
	Runs     []RunSummary   `json:"runs"`
}

// GetArticleDetail assembles the full review payload.
func (s *Store) GetArticleDetail(ctx context.Context, org, id string) (*ArticleDetail, error) {
	if err := validUUID(id); err != nil {
		return nil, err
	}
	d := &ArticleDetail{Claims: []Claim{}, Reviews: []Review{}, Pairs: []SentencePair{}, Runs: []RunSummary{}}
	var err error
	if d.Article, err = s.getArticle(ctx, s.db, org, id); err != nil {
		return nil, err
	}
	if d.Article.BriefID != "" {
		if d.Brief, err = s.getBrief(ctx, s.db, org, d.Article.BriefID); err != nil {
			return nil, err
		}
	}
	if d.Site, err = s.getSite(ctx, s.db, org, d.Article.SiteID); err != nil {
		return nil, err
	}
	if d.Article.CurrentRevisionID != "" {
		if d.Revision, err = s.getRevision(ctx, s.db, org, d.Article.CurrentRevisionID); err != nil {
			return nil, err
		}
		ids := map[string]bool{}
		var list []string
		for _, r := range d.Revision.ClaimRefs {
			if !ids[r.ClaimID] {
				ids[r.ClaimID] = true
				list = append(list, r.ClaimID)
			}
		}
		if len(list) > 0 {
			if d.Claims, err = s.ClaimsByIDs(ctx, org, list); err != nil {
				return nil, err
			}
		}
		byKey, latest, _ := IndexClaims(d.Claims)
		d.Pairs = BuildPairs(d.Revision.Package, d.Revision.ClaimRefs, d.Revision.Checks, byKey, latest)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, revision_id, revision_hash, reviewer, role, decision, findings, accepted_ids, COALESCE(minutes, 0)::float8, created_at
		FROM content_reviews
		WHERE org_id = $1 AND revision_id IN (SELECT id FROM content_revisions WHERE article_id = $2)
		ORDER BY created_at`, org, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rv Review
		var findings, accepted []byte
		if err := rows.Scan(&rv.ID, &rv.RevisionID, &rv.RevisionHash, &rv.Reviewer, &rv.Role, &rv.Decision, &findings, &accepted, &rv.Minutes, &rv.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(findings, &rv.Findings) // column is NOT NULL DEFAULT '[]'
		_ = json.Unmarshal(accepted, &rv.AcceptedIDs)
		d.Reviews = append(d.Reviews, rv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (stage) stage, status, attempts, COALESCE(error, ''), started_at, finished_at
		FROM content_pipeline_runs WHERE org_id = $1 AND article_id = $2
		ORDER BY stage, started_at DESC NULLS LAST`, org, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rs RunSummary
		var st, fin sql.NullTime
		if err := rows.Scan(&rs.Stage, &rs.Status, &rs.Attempts, &rs.Error, &st, &fin); err != nil {
			return nil, err
		}
		if st.Valid {
			rs.StartedAt = &st.Time
		}
		if fin.Valid {
			rs.FinishedAt = &fin.Time
		}
		d.Runs = append(d.Runs, rs)
	}
	return d, rows.Err()
}

// ── Claims ──────────────────────────────────────────────────────────────

const claimCols = `claim_id, version, text, type, source_url, passage, context, published_at, effective_at,
	retrieved_at, jurisdiction, population, conditions, status, derivation, calc, claim_key, question, answer, created_at`

func scanClaim(sc scanner) (Claim, error) {
	var c Claim
	var retrieved sql.NullTime
	var created time.Time
	var calc []byte
	err := sc.Scan(&c.ClaimID, &c.Version, &c.Text, &c.Type, &c.SourceURL, &c.Passage, &c.Context, &c.PublishedAt,
		&c.EffectiveAt, &retrieved, &c.Jurisdiction, &c.Population, &c.Conditions, &c.Status, &c.Derivation, &calc,
		&c.ClaimKey, &c.Question, &c.Answer, &created)
	if err != nil {
		return c, err
	}
	if retrieved.Valid {
		c.RetrievedAt = retrieved.Time.UTC().Format(time.RFC3339)
	}
	c.CreatedAt = created.UTC().Format(time.RFC3339)
	if len(calc) > 0 && string(calc) != "null" {
		var k Calc
		if err := json.Unmarshal(calc, &k); err == nil {
			c.Calc = &k
		}
	}
	return c, nil
}

// ClaimsByIDs returns every version of the given claims.
func (s *Store) ClaimsByIDs(ctx context.Context, org string, ids []string) ([]Claim, error) {
	out := []Claim{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+claimCols+` FROM content_claims
		WHERE org_id = $1 AND claim_id = ANY($2::uuid[]) ORDER BY claim_id, version`, org, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// IndexClaims keys claims by version and finds each claim's latest version.
func IndexClaims(all []Claim) (byKey map[string]Claim, latest map[string]int, latestClaim map[string]Claim) {
	byKey, latest, latestClaim = map[string]Claim{}, map[string]int{}, map[string]Claim{}
	for _, c := range all {
		byKey[ClaimVersionKey(c.ClaimID, c.Version)] = c
		if c.Version > latest[c.ClaimID] {
			latest[c.ClaimID] = c.Version
			latestClaim[c.ClaimID] = c
		}
	}
	return
}

func insertClaim(ctx context.Context, q querier, org, originArticle string, c Claim) error {
	var calc any
	if c.Calc != nil {
		calc = mustJSON(c.Calc)
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO content_claims (claim_id, version, org_id, origin_article_id, claim_key, question, answer, text, type,
			source_url, passage, context, published_at, effective_at, retrieved_at, jurisdiction, population, conditions,
			status, derivation, calc)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW(), $15, $16, $17, $18, $19, $20::jsonb)`,
		c.ClaimID, c.Version, org, originArticle, c.ClaimKey, c.Question, c.Answer, c.Text, c.Type,
		c.SourceURL, c.Passage, c.Context, c.PublishedAt, c.EffectiveAt, c.Jurisdiction, c.Population, c.Conditions,
		c.Status, c.Derivation, calc)
	return err
}

// InsertClaims appends new claims (version 1, fresh ids) in one transaction.
func (s *Store) InsertClaims(ctx context.Context, org, articleID string, claims []Claim) ([]Claim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	out := make([]Claim, len(claims))
	for i, c := range claims {
		if c.ClaimID == "" {
			c.ClaimID = uuid.NewString()
		}
		if c.Version == 0 {
			c.Version = 1
		}
		if err := insertClaim(ctx, tx, org, articleID, c); err != nil {
			return nil, err
		}
		out[i] = c
	}
	return out, tx.Commit()
}

func validateClaim(c Claim) error {
	if strings.TrimSpace(c.Text) == "" {
		return fmt.Errorf("%w: claim text is required", ErrInvalid)
	}
	switch c.Type {
	case ClaimSourcedFact, ClaimCalculation, ClaimAssumption, ClaimInterpretation:
	default:
		return fmt.Errorf("%w: type must be sourced_fact|calculation|assumption|interpretation", ErrInvalid)
	}
	switch c.Status {
	case ClaimSupported, ClaimInsufficientEvidence, ClaimConflicting:
	default:
		return fmt.Errorf("%w: status must be supported|insufficient_evidence|conflicting", ErrInvalid)
	}
	if c.Derivation != DerivationPrimary && c.Derivation != DerivationRederive {
		return fmt.Errorf("%w: derivation must be primary|rederive", ErrInvalid)
	}
	return nil
}

// flagStaleArticlesSQL: every article whose CURRENT revision references an
// older version of the claim goes back to changes_requested (from any
// reviewed/published state) and loses harvestability.
const flagStaleArticlesSQL = `
	UPDATE content_articles a SET
		status = CASE WHEN a.status IN ('in_review', 'approved', 'publishing', 'live_confirmed')
		              THEN 'changes_requested' ELSE a.status END,
		harvestable = FALSE,
		updated_at = NOW()
	FROM content_revisions r
	WHERE r.id = a.current_revision_id AND a.org_id = $1
	  AND EXISTS (SELECT 1 FROM jsonb_array_elements(r.claim_refs) e
	              WHERE e->>'claim_id' = $2 AND (e->>'version')::int < $3)
	RETURNING a.id`

// NewClaimVersion appends version max+1 of a claim and invalidates every
// article referencing an older version. claim_key/question carry over when
// omitted.
func (s *Store) NewClaimVersion(ctx context.Context, org, claimID string, in Claim) (Claim, []string, error) {
	if err := validUUID(claimID); err != nil {
		return in, nil, err
	}
	if in.Derivation == "" {
		in.Derivation = DerivationPrimary
	}
	if err := validateClaim(in); err != nil {
		return in, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return in, nil, err
	}
	defer rollback(tx)
	var maxV int
	var key, question, origin string
	err = tx.QueryRowContext(ctx, `
		SELECT version, claim_key, question, COALESCE(origin_article_id::text, '')
		FROM content_claims WHERE org_id = $1 AND claim_id = $2
		ORDER BY version DESC LIMIT 1`, org, claimID).Scan(&maxV, &key, &question, &origin)
	if errors.Is(err, sql.ErrNoRows) {
		return in, nil, ErrNotFound
	}
	if err != nil {
		return in, nil, err
	}
	in.ClaimID, in.Version = claimID, maxV+1
	if in.ClaimKey == "" {
		in.ClaimKey = key
	}
	if in.Question == "" {
		in.Question = question
	}
	if err := insertClaim(ctx, tx, org, origin, in); err != nil {
		if isUniqueViolation(err) {
			return in, nil, fmt.Errorf("%w: concurrent version bump on claim %s", ErrConflict, claimID)
		}
		return in, nil, err
	}
	rows, err := tx.QueryContext(ctx, flagStaleArticlesSQL, org, claimID, in.Version)
	if err != nil {
		return in, nil, err
	}
	flagged := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return in, nil, err
		}
		flagged = append(flagged, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, nil, err
	}
	return in, flagged, tx.Commit()
}

// ── Reviews ─────────────────────────────────────────────────────────────

// SubmitReview records a review bound to a revision hash and applies the
// resulting status. A hash that is not the current revision's is refused
// with ErrHashMismatch before anything is written; a blocked approve writes
// nothing and returns the blockers.
func (s *Store) SubmitReview(ctx context.Context, org, articleID string, in ReviewInput) (Review, ReviewOutcome, error) {
	var rv Review
	if err := validUUID(articleID); err != nil {
		return rv, ReviewOutcome{}, err
	}
	if err := in.Validate(); err != nil {
		return rv, ReviewOutcome{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rv, ReviewOutcome{}, err
	}
	defer rollback(tx)
	var status, revID, revHash string
	var checksRaw []byte
	var consequential bool
	err = tx.QueryRowContext(ctx, `
		SELECT a.status, COALESCE(r.id::text, ''), COALESCE(r.revision_hash, ''), COALESCE(r.checks, '{}'::jsonb),
		       COALESCE(b.consequential, FALSE)
		FROM content_articles a
		LEFT JOIN content_briefs b ON b.id = a.brief_id
		LEFT JOIN content_revisions r ON r.id = a.current_revision_id
		WHERE a.id = $1 AND a.org_id = $2
		FOR UPDATE OF a`, articleID, org).Scan(&status, &revID, &revHash, &checksRaw, &consequential)
	if errors.Is(err, sql.ErrNoRows) {
		return rv, ReviewOutcome{}, ErrNotFound
	}
	if err != nil {
		return rv, ReviewOutcome{}, err
	}
	if revHash == "" || revHash != in.RevisionHash {
		return rv, ReviewOutcome{}, fmt.Errorf("%w (current %q)", ErrHashMismatch, revHash)
	}
	if status != StatusInReview {
		return rv, ReviewOutcome{}, fmt.Errorf("%w: article is %q, not in_review", ErrConflict, status)
	}
	var checks Checks
	if err := json.Unmarshal(checksRaw, &checks); err != nil {
		return rv, ReviewOutcome{}, fmt.Errorf("revision checks unreadable: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT reviewer, role, decision, findings FROM content_reviews WHERE revision_id = $1 ORDER BY created_at`, revID)
	if err != nil {
		return rv, ReviewOutcome{}, err
	}
	var prior []PriorReview
	for rows.Next() {
		var p PriorReview
		var f []byte
		if err := rows.Scan(&p.Reviewer, &p.Role, &p.Decision, &f); err != nil {
			rows.Close()
			return rv, ReviewOutcome{}, err
		}
		_ = json.Unmarshal(f, &p.Findings)
		prior = append(prior, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rv, ReviewOutcome{}, err
	}
	outcome := DecideReview(checks, consequential, prior, in)
	if in.Decision == "approve" && len(outcome.Blockers) > 0 {
		return rv, outcome, ErrApprovalBlocked
	}
	findings := in.Findings
	if findings == nil {
		findings = []Finding{}
	}
	accepted := in.AcceptedIDs
	if accepted == nil {
		accepted = []string{}
	}
	rv = Review{RevisionID: revID, RevisionHash: revHash, Reviewer: in.Reviewer, Role: in.Role, Decision: in.Decision,
		Findings: findings, AcceptedIDs: accepted, Minutes: in.Minutes}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO content_reviews (org_id, revision_id, revision_hash, reviewer, role, decision, findings, accepted_ids, minutes)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9)
		RETURNING id, created_at`,
		org, revID, revHash, in.Reviewer, in.Role, in.Decision, mustJSON(findings), mustJSON(accepted), in.Minutes,
	).Scan(&rv.ID, &rv.CreatedAt); err != nil {
		return rv, outcome, err
	}
	if outcome.Status != status {
		if _, err := tx.ExecContext(ctx, `UPDATE content_articles SET status = $1, updated_at = NOW() WHERE id = $2 AND org_id = $3`,
			outcome.Status, articleID, org); err != nil {
			return rv, outcome, err
		}
	}
	return rv, outcome, tx.Commit()
}

// ── Releases ────────────────────────────────────────────────────────────

func (s *Store) manifest(ctx context.Context, q querier, org, siteID string) ([]ManifestEntry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT a.id, r.id, r.revision_hash, a.slug
		FROM content_articles a JOIN content_revisions r ON r.id = a.current_revision_id
		WHERE a.org_id = $1 AND a.site_id = $2 AND a.status IN ('approved', 'publishing', 'live_confirmed')
		ORDER BY a.slug, a.id`, org, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManifestEntry{}
	for rows.Next() {
		var e ManifestEntry
		if err := rows.Scan(&e.ArticleID, &e.RevisionID, &e.RevisionHash, &e.Slug); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Manifest is the approved-and-intended set for a site.
func (s *Store) Manifest(ctx context.Context, org, siteID string) ([]ManifestEntry, string, error) {
	if err := validUUID(siteID); err != nil {
		return nil, "", err
	}
	m, err := s.manifest(ctx, s.db, org, siteID)
	if err != nil {
		return nil, "", err
	}
	h, err := ManifestHash(m)
	return m, h, err
}

// Release is one content_releases row.
type Release struct {
	ID           string          `json:"id"`
	SiteID       string          `json:"site_id"`
	Manifest     []ManifestEntry `json:"manifest"`
	ManifestHash string          `json:"manifest_hash"`
	Status       string          `json:"status"`
	Error        string          `json:"error,omitempty"`
	StartedAt    time.Time       `json:"started_at"`
	FinishedAt   *time.Time      `json:"finished_at"`
}

// CreateRelease snapshots the current manifest under a per-site transaction
// lock. It refuses when a release is already in flight for the site, when
// the manifest is empty, and when the manifest DROPS an article from the last
// live_confirmed release that was not withdrawn.
func (s *Store) CreateRelease(ctx context.Context, org, siteID string) (Release, error) {
	rel := Release{SiteID: siteID, Status: ReleasePending}
	if err := validUUID(siteID); err != nil {
		return rel, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rel, err
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "content_release:"+siteID); err != nil {
		return rel, err
	}
	var inflight int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_releases
		WHERE org_id = $1 AND site_id = $2 AND status IN ('pending', 'building', 'deployed')`, org, siteID).Scan(&inflight); err != nil {
		return rel, err
	}
	if inflight > 0 {
		return rel, fmt.Errorf("%w: a release is already in flight for this site", ErrConflict)
	}
	entries, err := s.manifest(ctx, tx, org, siteID)
	if err != nil {
		return rel, err
	}
	var prevRaw []byte
	var prev []ManifestEntry
	err = tx.QueryRowContext(ctx, `SELECT manifest FROM content_releases
		WHERE org_id = $1 AND site_id = $2 AND status = 'live_confirmed'
		ORDER BY finished_at DESC NULLS LAST, started_at DESC LIMIT 1`, org, siteID).Scan(&prevRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return rel, err
	}
	if len(prevRaw) > 0 {
		if err := json.Unmarshal(prevRaw, &prev); err != nil {
			return rel, fmt.Errorf("previous live manifest unreadable: %w", err)
		}
	}
	// An empty manifest is a valid TAKEDOWN when something is live (the last
	// article was withdrawn); with nothing live it is a no-op, refused.
	if len(entries) == 0 && len(prev) == 0 {
		return rel, fmt.Errorf("%w: manifest is empty and nothing is live", ErrInvalid)
	}
	withdrawn := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM content_articles WHERE org_id = $1 AND site_id = $2 AND status = 'withdrawn'`, org, siteID)
	if err != nil {
		return rel, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return rel, err
		}
		withdrawn[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rel, err
	}
	if v := ManifestDropViolations(prev, entries, withdrawn); len(v) > 0 {
		return rel, fmt.Errorf("%w: %s", ErrManifestDrop, strings.Join(v, "; "))
	}
	rel.Manifest = entries
	if rel.ManifestHash, err = ManifestHash(entries); err != nil {
		return rel, err
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO content_releases (org_id, site_id, manifest, manifest_hash, status, started_at)
		VALUES ($1, $2, $3::jsonb, $4, 'pending', NOW()) RETURNING id, started_at`,
		org, siteID, mustJSON(entries), rel.ManifestHash).Scan(&rel.ID, &rel.StartedAt); err != nil {
		if isUniqueViolation(err) {
			return rel, fmt.Errorf("%w: a release is already in flight for this site", ErrConflict)
		}
		return rel, err
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ArticleID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE content_articles SET status = 'publishing', updated_at = NOW()
		WHERE org_id = $1 AND site_id = $2 AND status = 'approved' AND id = ANY($3::uuid[])`, org, siteID, pq.Array(ids)); err != nil {
		return rel, err
	}
	return rel, tx.Commit()
}

// SetReleaseStatus advances a release. live_confirmed marks harvestable ONLY
// the article revisions in that manifest (and only if still current); failed
// returns publishing articles to approved.
func (s *Store) SetReleaseStatus(ctx context.Context, org, id, to, errMsg string) (Release, error) {
	rel := Release{ID: id}
	if err := validUUID(id); err != nil {
		return rel, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rel, err
	}
	defer rollback(tx)
	var raw []byte
	var from string
	err = tx.QueryRowContext(ctx, `SELECT site_id, status, manifest, manifest_hash, started_at
		FROM content_releases WHERE id = $1 AND org_id = $2 FOR UPDATE`, id, org).
		Scan(&rel.SiteID, &from, &raw, &rel.ManifestHash, &rel.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return rel, ErrNotFound
	}
	if err != nil {
		return rel, err
	}
	if !ValidReleaseTransition(from, to) {
		return rel, fmt.Errorf("%w: release cannot go %s → %s", ErrConflict, from, to)
	}
	if err := json.Unmarshal(raw, &rel.Manifest); err != nil {
		return rel, fmt.Errorf("manifest unreadable: %w", err)
	}
	var fin sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		UPDATE content_releases SET status = $1, error = NULLIF($2, ''),
			finished_at = CASE WHEN $1 IN ('live_confirmed', 'failed') THEN NOW() ELSE finished_at END
		WHERE id = $3 RETURNING finished_at`, to, errMsg, id).Scan(&fin); err != nil {
		return rel, err
	}
	if fin.Valid {
		rel.FinishedAt = &fin.Time
	}
	rel.Status, rel.Error = to, errMsg
	artIDs := make([]string, len(rel.Manifest))
	revIDs := make([]string, len(rel.Manifest))
	for i, e := range rel.Manifest {
		artIDs[i], revIDs[i] = e.ArticleID, e.RevisionID
	}
	switch to {
	case ReleaseLiveConfirmed:
		if _, err := tx.ExecContext(ctx, `
			UPDATE content_articles a SET status = 'live_confirmed', harvestable = TRUE, updated_at = NOW()
			FROM unnest($2::uuid[], $3::uuid[]) AS m(article_id, revision_id)
			WHERE a.org_id = $1 AND a.id = m.article_id AND a.current_revision_id = m.revision_id
			  AND a.status IN ('publishing', 'approved', 'live_confirmed')`, org, pq.Array(artIDs), pq.Array(revIDs)); err != nil {
			return rel, err
		}
	case ReleaseFailed:
		if _, err := tx.ExecContext(ctx, `UPDATE content_articles SET status = 'approved', updated_at = NOW()
			WHERE org_id = $1 AND id = ANY($2::uuid[]) AND status = 'publishing'`, org, pq.Array(artIDs)); err != nil {
			return rel, err
		}
	}
	return rel, tx.Commit()
}

// Withdraw stops harvestability FIRST, then marks the article withdrawn so
// the next manifest omits it (the drop guard allows withdrawn articles).
// Returns the site that needs a release.
func (s *Store) Withdraw(ctx context.Context, org, id string) (string, error) {
	if err := validUUID(id); err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer rollback(tx)
	var siteID string
	err = tx.QueryRowContext(ctx, `UPDATE content_articles SET harvestable = FALSE, updated_at = NOW()
		WHERE id = $1 AND org_id = $2 AND status <> 'withdrawn' RETURNING site_id`, id, org).Scan(&siteID)
	if errors.Is(err, sql.ErrNoRows) {
		var n int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_articles WHERE id = $1 AND org_id = $2`, id, org).Scan(&n); e != nil {
			return "", e
		}
		if n == 0 {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("%w: already withdrawn", ErrConflict)
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE content_articles SET status = 'withdrawn', withdrawn_at = NOW(),
		run_requested_at = NULL, updated_at = NOW() WHERE id = $1 AND org_id = $2`, id, org); err != nil {
		return "", err
	}
	return siteID, tx.Commit()
}

// ── Budget (implements BudgetLedger) ────────────────────────────────────

const denverToday = `(NOW() AT TIME ZONE 'America/Denver')::date`

// SpentToday is today's (Denver) spend.
func (s *Store) SpentToday(ctx context.Context, org string) (float64, error) {
	var v float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT usd FROM content_usage_daily
		WHERE org_id = $1 AND day = `+denverToday+`), 0)::float8`, org).Scan(&v)
	return v, err
}

// AddSpend adds to today's spend. Runs detached from ctx cancellation: the
// money is spent once the response arrived.
func (s *Store) AddSpend(ctx context.Context, org string, usd float64) error {
	if usd <= 0 {
		return nil
	}
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(c, `
		INSERT INTO content_usage_daily (org_id, day, usd, updated_at) VALUES ($1, `+denverToday+`, $2, NOW())
		ON CONFLICT (org_id, day) DO UPDATE SET usd = content_usage_daily.usd + EXCLUDED.usd, updated_at = NOW()`, org, usd)
	return err
}

// ── Pipeline runs ───────────────────────────────────────────────────────

// RunClaim is the result of claiming a stage run: either a fresh run id, or
// the stored output of an earlier successful run (Prior).
type RunClaim struct {
	RunID string
	Prior json.RawMessage
}

// FailMode says how a failed attempt counts against the retry budget.
type FailMode int

const (
	FailCount    FailMode = iota // a normal failure consumes the attempt
	FailTerminal                 // refusal / no allow-list: exhaust now
	FailRefund                   // kill switch / budget / cancellation: not the content's fault
)

// ClaimRun is the idempotency gate for one (article, stage, input_hash):
// INSERT … ON CONFLICT DO NOTHING wins the run; a loser sees the existing row
// and reuses its output (succeeded), backs off (running — double fire), or
// atomically retakes it (failed with attempts left / running but stale).
func (s *Store) ClaimRun(ctx context.Context, org, articleID, stage, inputHash string, maxAttempts int, staleAfter time.Duration) (RunClaim, error) {
	var rc RunClaim
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO content_pipeline_runs (org_id, article_id, stage, input_hash, status, attempts, started_at)
		VALUES ($1, $2, $3, $4, 'running', 1, NOW())
		ON CONFLICT (article_id, stage, input_hash) DO NOTHING
		RETURNING id`, org, articleID, stage, inputHash).Scan(&rc.RunID)
	if err == nil {
		return rc, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return rc, err
	}
	var id, status string
	var attempts int
	var output []byte
	var stale bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, status, attempts, COALESCE(output, 'null'::jsonb),
		       COALESCE(started_at < NOW() - $4::float8 * INTERVAL '1 second', FALSE)
		FROM content_pipeline_runs WHERE article_id = $1 AND stage = $2 AND input_hash = $3`,
		articleID, stage, inputHash, staleAfter.Seconds()).Scan(&id, &status, &attempts, &output, &stale); err != nil {
		return rc, err
	}
	switch status {
	case "succeeded":
		rc.RunID, rc.Prior = id, output
		return rc, nil
	case "running":
		if !stale {
			return rc, ErrInFlight
		}
		if attempts >= maxAttempts {
			return rc, ErrRunExhausted
		}
		err = s.db.QueryRowContext(ctx, `UPDATE content_pipeline_runs SET attempts = attempts + 1, started_at = NOW()
			WHERE id = $1 AND status = 'running' AND started_at < NOW() - $2::float8 * INTERVAL '1 second'
			RETURNING id`, id, staleAfter.Seconds()).Scan(&rc.RunID)
	default: // failed
		if attempts >= maxAttempts {
			return rc, ErrRunExhausted
		}
		err = s.db.QueryRowContext(ctx, `UPDATE content_pipeline_runs SET status = 'running', attempts = attempts + 1,
			started_at = NOW(), error = NULL WHERE id = $1 AND status = 'failed' RETURNING id`, id).Scan(&rc.RunID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return rc, ErrInFlight // another worker won the retake
	}
	return rc, err
}

// FinishRun stores a successful stage output.
func (s *Store) FinishRun(ctx context.Context, runID string, output []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE content_pipeline_runs SET status = 'succeeded', output = $2::jsonb,
		error = NULL, finished_at = NOW() WHERE id = $1`, runID, output)
	return err
}

// FailRun records a failed attempt.
func (s *Store) FailRun(ctx context.Context, runID, msg string, mode FailMode, maxAttempts int) error {
	var q string
	args := []any{runID, clip(msg, 2000)}
	switch mode {
	case FailTerminal:
		q = `UPDATE content_pipeline_runs SET status = 'failed', error = $2, attempts = GREATEST(attempts, $3), finished_at = NOW() WHERE id = $1`
		args = append(args, maxAttempts)
	case FailRefund:
		q = `UPDATE content_pipeline_runs SET status = 'failed', error = $2, attempts = GREATEST(attempts - 1, 0), finished_at = NOW() WHERE id = $1`
	default:
		q = `UPDATE content_pipeline_runs SET status = 'failed', error = $2, finished_at = NOW() WHERE id = $1`
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// PendingArticle is a queued article.
type PendingArticle struct {
	OrgID     string
	ArticleID string
}

// PendingArticles lists queued articles across orgs, oldest request first.
func (s *Store) PendingArticles(ctx context.Context, limit int) ([]PendingArticle, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT org_id, id FROM content_articles
		WHERE run_requested_at IS NOT NULL AND status = 'drafting'
		ORDER BY run_requested_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingArticle
	for rows.Next() {
		var p PendingArticle
		if err := rows.Scan(&p.OrgID, &p.ArticleID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PipelineInput is what the stages read.
type PipelineInput struct {
	OrgID   string
	Article Article
	Brief   Brief
	Site    Site
}

// LoadPipelineInput loads a drafting article with its brief and site.
func (s *Store) LoadPipelineInput(ctx context.Context, org, articleID string) (PipelineInput, error) {
	in := PipelineInput{OrgID: org}
	var err error
	if in.Article, err = s.getArticle(ctx, s.db, org, articleID); err != nil {
		return in, err
	}
	if in.Article.Status != StatusDrafting {
		return in, fmt.Errorf("%w: article is %q, not drafting", ErrConflict, in.Article.Status)
	}
	if in.Article.BriefID == "" {
		return in, fmt.Errorf("%w: article has no brief", ErrInvalid)
	}
	if in.Brief, err = s.getBrief(ctx, s.db, org, in.Article.BriefID); err != nil {
		return in, err
	}
	in.Site, err = s.getSite(ctx, s.db, org, in.Article.SiteID)
	return in, err
}

// TakenSlugs is every other slug on the site: content-desk articles plus the
// site's existing slugs when its adapter carries them (adapter.existing_slugs).
func (s *Store) TakenSlugs(ctx context.Context, org, siteID, articleID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT slug FROM content_articles WHERE org_id = $1 AND site_id = $2 AND id <> $3
		UNION ALL
		SELECT jsonb_array_elements_text(CASE WHEN jsonb_typeof(adapter->'existing_slugs') = 'array'
		                                      THEN adapter->'existing_slugs' ELSE '[]'::jsonb END)
		FROM content_sites WHERE org_id = $1 AND id = $2`, org, siteID, articleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// OtherBodies returns the current-revision body text of up to 500 other
// articles on the site (near-duplicate heuristic input).
func (s *Store) OtherBodies(ctx context.Context, org, siteID, articleID string) ([]OtherArticle, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, r.package FROM content_articles a JOIN content_revisions r ON r.id = a.current_revision_id
		WHERE a.org_id = $1 AND a.site_id = $2 AND a.id <> $3
		ORDER BY a.updated_at DESC LIMIT 500`, org, siteID, articleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OtherArticle
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var p Package
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		out = append(out, OtherArticle{ArticleID: id, Body: PackageBodyText(p)})
	}
	return out, rows.Err()
}

// SaveRevision writes the revision (idempotent on (article, hash)) and moves
// the article drafting → in_review.
func (s *Store) SaveRevision(ctx context.Context, org, articleID string, pkg Package, refs []ClaimRef, checks Checks, usage Usage) (string, string, error) {
	hash, err := RevisionHash(pkg, refs)
	if err != nil {
		return "", "", err
	}
	if checks.Code == nil {
		checks.Code = []CheckResult{}
	}
	if checks.Judgment == nil {
		checks.Judgment = []JudgmentItem{}
	}
	sorted := SortClaimRefs(refs)
	if sorted == nil {
		sorted = []ClaimRef{}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", hash, err
	}
	defer rollback(tx)
	var revID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO content_revisions (org_id, article_id, revision_hash, package, claim_refs, checks, usage)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6::jsonb, $7::jsonb)
		ON CONFLICT (article_id, revision_hash) DO UPDATE SET checks = EXCLUDED.checks, usage = EXCLUDED.usage
		RETURNING id`, org, articleID, hash, mustJSON(pkg), mustJSON(sorted), mustJSON(checks), mustJSON(usage)).Scan(&revID); err != nil {
		return "", hash, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE content_articles SET current_revision_id = $1, status = 'in_review',
		run_requested_at = NULL, updated_at = NOW() WHERE id = $2 AND org_id = $3 AND status = 'drafting'`, revID, articleID, org)
	if err != nil {
		return revID, hash, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return revID, hash, fmt.Errorf("%w: article left drafting while the pipeline ran", ErrConflict)
	}
	return revID, hash, tx.Commit()
}

// ── Ops status ──────────────────────────────────────────────────────────

// StageCount is pipeline runs per (stage, status).
type StageCount struct {
	Stage  string `json:"stage"`
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// ReleaseSummary is a site's latest release.
type ReleaseSummary struct {
	SiteID     string     `json:"site_id"`
	Domain     string     `json:"domain"`
	ReleaseID  string     `json:"release_id,omitempty"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// PipelineError is the most recent failed stage run.
type PipelineError struct {
	ArticleID string     `json:"article_id"`
	Stage     string     `json:"stage"`
	Error     string     `json:"error"`
	At        *time.Time `json:"at"`
}

// OpsStatus is the dashboard payload.
type OpsStatus struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Pipeline    []StageCount `json:"pipeline"`
	ReviewQueue struct {
		Size             int      `json:"size"`
		OldestAgeSeconds *float64 `json:"oldest_age_seconds"`
	} `json:"review_queue"`
	Releases []ReleaseSummary `json:"releases"`
	Spend    struct {
		Day       string  `json:"day"`
		TodayUSD  float64 `json:"today_usd"`
		BudgetUSD float64 `json:"budget_usd"`
		Exceeded  bool    `json:"exceeded"`
	} `json:"spend"`
	KillSwitch struct {
		Env     string `json:"env"`
		Enabled bool   `json:"enabled"`
	} `json:"kill_switch"`
	Models                       ModelConfig    `json:"models"`
	LastPipelineError            *PipelineError `json:"last_pipeline_error"`
	NewsletterSupplyRunway       *float64       `json:"newsletter_supply_runway"`
	NewsletterSupplyRunwayReason string         `json:"newsletter_supply_runway_reason"`
	// Checks: server-side checks + the latest Mac-side ops reports (ops.go).
	Checks []OpsCheck `json:"checks"`
	Supply struct {
		Consumers json.RawMessage `json:"consumers"` // payload.consumers of the supply_runway report; null when absent
	} `json:"supply"`
}

// RunwayUnavailableReason explains the null runway.
const RunwayUnavailableReason = "the newsletter article harvest pool is laptop-local (.scratch/brand_articles.json written by agents/content/harvest.py), not in Postgres — the server cannot read it"

// OpsStatus assembles the dashboard JSON from five small queries.
func (s *Store) OpsStatus(ctx context.Context, org string) (*OpsStatus, error) {
	o := &OpsStatus{GeneratedAt: time.Now().UTC(), Pipeline: []StageCount{}, Releases: []ReleaseSummary{},
		Models: ModelsFromEnv(), NewsletterSupplyRunwayReason: RunwayUnavailableReason}
	o.KillSwitch.Env, o.KillSwitch.Enabled = EnvEnabled, Enabled()
	o.Spend.BudgetUSD = DailyBudgetUSD()

	rows, err := s.db.QueryContext(ctx, `SELECT stage, status, COUNT(*) FROM content_pipeline_runs
		WHERE org_id = $1 GROUP BY stage, status ORDER BY stage, status`, org)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c StageCount
		if err := rows.Scan(&c.Stage, &c.Status, &c.Count); err != nil {
			rows.Close()
			return nil, err
		}
		o.Pipeline = append(o.Pipeline, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var oldest sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), EXTRACT(EPOCH FROM NOW() - MIN(updated_at))::float8
		FROM content_articles WHERE org_id = $1 AND status = 'in_review'`, org).Scan(&o.ReviewQueue.Size, &oldest); err != nil {
		return nil, err
	}
	if oldest.Valid {
		v := oldest.Float64
		o.ReviewQueue.OldestAgeSeconds = &v
	}

	rows, err = s.db.QueryContext(ctx, `
		SELECT s.id, s.domain, COALESCE(r.id::text, ''), COALESCE(r.status, ''), COALESCE(r.error, ''), r.started_at, r.finished_at
		FROM content_sites s
		LEFT JOIN LATERAL (SELECT id, status, error, started_at, finished_at FROM content_releases
		                   WHERE site_id = s.id AND org_id = $1 ORDER BY started_at DESC LIMIT 1) r ON TRUE
		WHERE s.org_id = $1 ORDER BY s.domain`, org)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rs ReleaseSummary
		var st, fin sql.NullTime
		if err := rows.Scan(&rs.SiteID, &rs.Domain, &rs.ReleaseID, &rs.Status, &rs.Error, &st, &fin); err != nil {
			rows.Close()
			return nil, err
		}
		if st.Valid {
			rs.StartedAt = &st.Time
		}
		if fin.Valid {
			rs.FinishedAt = &fin.Time
		}
		o.Releases = append(o.Releases, rs)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := s.db.QueryRowContext(ctx, `SELECT to_char(`+denverToday+`, 'YYYY-MM-DD'),
		COALESCE((SELECT usd FROM content_usage_daily WHERE org_id = $1 AND day = `+denverToday+`), 0)::float8`, org).
		Scan(&o.Spend.Day, &o.Spend.TodayUSD); err != nil {
		return nil, err
	}
	o.Spend.Exceeded = o.Spend.TodayUSD >= o.Spend.BudgetUSD

	var pe PipelineError
	var at sql.NullTime
	err = s.db.QueryRowContext(ctx, `SELECT article_id, stage, COALESCE(error, ''), finished_at FROM content_pipeline_runs
		WHERE org_id = $1 AND status = 'failed' ORDER BY finished_at DESC NULLS LAST LIMIT 1`, org).
		Scan(&pe.ArticleID, &pe.Stage, &pe.Error, &at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if at.Valid {
			pe.At = &at.Time
		}
		o.LastPipelineError = &pe
	}
	return o, nil
}
