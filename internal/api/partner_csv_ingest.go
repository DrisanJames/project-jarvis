package api

// Operator CSV ingestion for partner datasets.
//
// POST /api/mailing/partner-ingest-csv/preview  (multipart: file, dataset_id)
// POST /api/mailing/partner-ingest-csv/commit   (multipart: file, dataset_id, mapping)
//
// The API door (POST /api/partner-ingest/v1/records) requires the partner to
// integrate; plenty of feeds arrive as a one-off CSV instead. This surface
// lets the operator upload a CSV against an existing dataset:
//
//   preview — parse, detect the header row, auto-suggest a column→field
//     mapping against the CLOSED ingestRecord struct (the canonical shape —
//     partner_ingest_handlers.go), and stream the WHOLE file once for
//     row_count / invalid emails / the per-ISP mix (isp.Group over the
//     mapped email column). Nothing is persisted.
//
//   commit — stream rows through the operator-confirmed mapping into
//     canonical ingestRecords (unmapped columns land in metadata, the ONLY
//     map that survives the closed-struct re-marshal), skip invalid emails,
//     chunk into ≤maxRecordsPerBatch batches, and persist each chunk through the
//     EXACT same path the API door uses — persistPartnerBatch (S3 NDJSON.gz +
//     .meta.json + partner_inbound_batches 'received') — so the slicer and EO
//     validator take over identically. partner_inbound_batches has no source
//     column, so the batch is marked via ingest_metadata.source='csv_upload'.
//
// Auth: mounted inside the authenticated /api router (session / X-Admin-Key)
// — this is an OPERATOR surface, not a partner one; no X-Partner-Key.
// Org: the partner tables carry no organization_id column (see
// property_lane_journey.go's scope note) — getOrgID(r) is echoed back.

import (
	"bufio"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// partnerCSVMaxBytes caps the multipart body. Package var (not const) so the
// oversize-reject test can exercise the cap without a 50MB fixture.
var partnerCSVMaxBytes int64 = 50 * 1024 * 1024

// partnerCSVMaxRecordsPerBatch mirrors the API door's maxRecordsPerBatch —
// package var for the chunking test.
var partnerCSVMaxRecordsPerBatch = maxRecordsPerBatch

const partnerCSVSampleRows = 10

// PartnerCSVIngestService is the operator CSV upload surface.
type PartnerCSVIngestService struct {
	db *sql.DB
	s3 *PartnerIngestS3Client
}

func NewPartnerCSVIngestService(db *sql.DB, s3 *PartnerIngestS3Client) *PartnerCSVIngestService {
	return &PartnerCSVIngestService{db: db, s3: s3}
}

// SetS3Client allows late wiring during boot (same as PartnerIngestHandler).
func (s *PartnerCSVIngestService) SetS3Client(c *PartnerIngestS3Client) { s.s3 = c }

// RegisterRoutes mounts under the /api/mailing router.
func (s *PartnerCSVIngestService) RegisterRoutes(r chi.Router) {
	r.Route("/partner-ingest-csv", func(cr chi.Router) {
		cr.Post("/preview", s.HandlePreview)
		cr.Post("/commit", s.HandleCommit)
	})
}

// ── column→field mapping ────────────────────────────────────────────────────
//
// Targets are the CLOSED ingestRecord struct's fields (the true field list —
// read that struct, partner_ingest_handlers.go:80). `tid` and `phone` have no
// struct column: tid folds into metadata["tid"] (the ONE spot the drip reads
// it back out of — see the UnmarshalJSON commentary), phone into
// metadata["phone"]. Any column mapped to a "metadata.<key>" target — or not
// mapped at all — survives as metadata, because metadata is the ONLY map the
// closed-struct re-marshal preserves.
//
// The alias vocabulary borrows the import_templates.go style but targets
// ingestRecord fields, folding in the partner spellings ingestRecord's own
// UnmarshalJSON already accepts (postal_code, zip_code, signup_ip, opt_in_ip,
// opt_in_url, var1, …).

type csvTargetSpec struct {
	Target  string
	Aliases []string
}

var csvCanonicalTargets = []csvTargetSpec{
	{"email", []string{"e_mail", "emailaddress", "email_address", "mail", "email_addr"}},
	{"first_name", []string{"firstname", "first", "given_name", "fname"}},
	{"last_name", []string{"lastname", "last", "surname", "family_name", "lname"}},
	{"city", []string{"town"}},
	{"zip", []string{"postal_code", "zipcode", "zip_code", "postcode", "postal"}},
	{"state", []string{"state_upper", "st", "region", "province", "long_state"}},
	{"address_1", []string{"address", "address1", "addr1", "street", "street_address"}},
	{"ip_address", []string{"ip", "ipaddress", "ip_addr", "signup_ip", "opt_in_ip", "optin_ip"}},
	{"opt_in_date", []string{"optin_date", "opt_in_at", "optindate", "opt_in", "opt_in_timestamp"}},
	{"signup_url", []string{"opt_in_url", "optin_url", "signup_link", "source_url", "url"}},
	{"signup_date", []string{"signup_at", "signupdate", "sign_up_date", "created_at", "date_added"}},
	{"source", []string{"lead_source", "sub_source", "source_id", "subid", "sub_id"}},
	{"metadata.tid", []string{"tid", "var1", "token", "tracking_id"}},
	{"metadata.phone", []string{"phone", "phone_number", "mobile", "tel", "telephone"}},
}

// csvFieldSetters routes a canonical target to its ingestRecord field.
var csvFieldSetters = map[string]func(*ingestRecord, string){
	"email":       func(r *ingestRecord, v string) { r.Email = v },
	"first_name":  func(r *ingestRecord, v string) { r.FirstName = v },
	"last_name":   func(r *ingestRecord, v string) { r.LastName = v },
	"city":        func(r *ingestRecord, v string) { r.City = v },
	"zip":         func(r *ingestRecord, v string) { r.Zip = v },
	"state":       func(r *ingestRecord, v string) { r.State = v },
	"address_1":   func(r *ingestRecord, v string) { r.Address1 = v },
	"ip_address":  func(r *ingestRecord, v string) { r.IPAddress = v },
	"opt_in_date": func(r *ingestRecord, v string) { r.OptInDate = v },
	"signup_url":  func(r *ingestRecord, v string) { r.SignupURL = v },
	"signup_date": func(r *ingestRecord, v string) { r.SignupAt = v },
	"source":      func(r *ingestRecord, v string) { r.Source = v },
}

// normalizeCSVHeader lowers, trims (incl. a UTF-8 BOM), and collapses
// spaces/dashes to underscores — the same normalization import_templates.go
// applies before its alias lookup.
func normalizeCSVHeader(h string) string {
	h = strings.TrimPrefix(h, "\uFEFF")
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.ReplaceAll(h, " ", "_")
	h = strings.ReplaceAll(h, "-", "_")
	return h
}

type csvMappingSuggestion struct {
	ColumnIndex int    `json:"column_index"`
	Header      string `json:"header"`
	// Target: a canonical ingestRecord field, "metadata.<key>" for columns
	// with no struct home, or "" when the header is empty.
	Target     string `json:"target"`
	Confidence string `json:"confidence"` // high | medium | none
}

// suggestCSVMapping maps headers → targets. Exact target-name match = high;
// alias match = medium; anything else = none, targeted at
// metadata.<normalized_header> so no partner column is silently destroyed.
func suggestCSVMapping(headers []string) []csvMappingSuggestion {
	direct := map[string]string{}
	alias := map[string]string{}
	for _, spec := range csvCanonicalTargets {
		// The bare metadata target names ("tid", "phone") count as direct.
		name := strings.TrimPrefix(spec.Target, "metadata.")
		direct[name] = spec.Target
		for _, a := range spec.Aliases {
			alias[a] = spec.Target
		}
	}
	out := make([]csvMappingSuggestion, 0, len(headers))
	for i, h := range headers {
		n := normalizeCSVHeader(h)
		sug := csvMappingSuggestion{ColumnIndex: i, Header: h}
		switch {
		case n == "":
			sug.Confidence = "none"
		case direct[n] != "":
			sug.Target = direct[n]
			sug.Confidence = "high"
		case alias[n] != "":
			sug.Target = alias[n]
			sug.Confidence = "medium"
		default:
			sug.Target = "metadata." + n
			sug.Confidence = "none"
		}
		out = append(out, sug)
	}
	return out
}

// csvLooksLikeHeader decides whether the first row is a header: no cell
// contains '@' (a data row's email column would), or at least one cell
// matches a known target/alias by name.
func csvLooksLikeHeader(row []string) bool {
	hasAt := false
	for _, c := range row {
		if strings.Contains(c, "@") {
			hasAt = true
			break
		}
	}
	if !hasAt {
		return true
	}
	for _, sug := range suggestCSVMapping(row) {
		if sug.Confidence == "high" || sug.Confidence == "medium" {
			return true
		}
	}
	return false
}

// csvValidEmail applies the same acceptance test as the API door's
// normalizeRecord (lower/trim, ≤254, contains '@', no spaces).
func csvValidEmail(raw string) (string, bool) {
	rec, ok := normalizeRecord(ingestRecord{Email: raw})
	return rec.Email, ok
}

// ── request plumbing ────────────────────────────────────────────────────────

// csvUpload is one parsed multipart request: the CSV reader plus form values.
type csvUpload struct {
	csv      *csv.Reader
	closeFn  func()
	dataset  string
	mapping  string
	filename string
}

// openCSVUpload enforces the byte cap and extracts file + fields. The file
// part is spooled by ParseMultipartForm (disk-backed above 16MiB) and then
// STREAMED row-by-row — parsed rows are never all buffered in memory.
func openCSVUpload(w http.ResponseWriter, r *http.Request) (*csvUpload, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, partnerCSVMaxBytes)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			respondError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file exceeds the %dMB upload cap — split it and upload in parts", partnerCSVMaxBytes/1024/1024))
			return nil, false
		}
		respondError(w, http.StatusBadRequest, "invalid multipart body: "+err.Error())
		return nil, false
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		respondError(w, http.StatusBadRequest, "multipart field 'file' is required")
		return nil, false
	}
	datasetID := strings.TrimSpace(r.FormValue("dataset_id"))
	if !isValidUUID(datasetID) {
		file.Close()
		respondError(w, http.StatusBadRequest, "dataset_id must be a valid dataset UUID")
		return nil, false
	}
	// BOM-tolerant buffered reader → csv.Reader. LazyQuotes + ragged rows
	// tolerated: a short row reads as empty cells, never a hard error.
	br := bufio.NewReader(file)
	if peek, _ := br.Peek(3); len(peek) == 3 && peek[0] == 0xEF && peek[1] == 0xBB && peek[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.TrimLeadingSpace = true
	return &csvUpload{
		csv:      cr,
		closeFn:  func() { file.Close() },
		dataset:  datasetID,
		mapping:  strings.TrimSpace(r.FormValue("mapping")),
		filename: hdr.Filename,
	}, true
}

// csvDatasetIdent validates the dataset: must exist, be active, and not be
// emergency-paused. (The partner tables carry no org column — org context is
// echoed by the handlers, not filtered here.)
type csvDatasetIdent struct {
	PartnerID   string
	PartnerSlug string
	DatasetSlug string
	DatasetName string
	Vertical    string
}

func (s *PartnerCSVIngestService) resolveDataset(w http.ResponseWriter, r *http.Request, datasetID string) (csvDatasetIdent, bool) {
	var (
		ident         csvDatasetIdent
		status        string
		paused        bool
		partnerStatus string
	)
	err := s.db.QueryRowContext(r.Context(), `
		SELECT d.partner_id, d.slug, d.name, d.vertical, COALESCE(d.status, ''),
		       COALESCE(d.paused_emergency, false),
		       p.slug, COALESCE(p.status, 'active')
		FROM partner_datasets d
		JOIN data_partners p ON p.id = d.partner_id
		WHERE d.id = $1
	`, datasetID).Scan(&ident.PartnerID, &ident.DatasetSlug, &ident.DatasetName, &ident.Vertical,
		&status, &paused, &ident.PartnerSlug, &partnerStatus)
	if errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusNotFound, "dataset not found")
		return ident, false
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "dataset lookup failed")
		return ident, false
	}
	if status != "active" || partnerStatus != "active" {
		respondError(w, http.StatusConflict, "dataset (or its partner) is not active")
		return ident, false
	}
	if paused {
		respondError(w, http.StatusConflict, "dataset is emergency-paused — resume it before uploading")
		return ident, false
	}
	return ident, true
}

// ── POST /preview ───────────────────────────────────────────────────────────

func (s *PartnerCSVIngestService) HandlePreview(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	up, ok := openCSVUpload(w, r)
	if !ok {
		return
	}
	defer up.closeFn()
	if _, ok := s.resolveDataset(w, r, up.dataset); !ok {
		return
	}

	first, err := up.csv.Read()
	if err != nil {
		respondError(w, http.StatusBadRequest, "file is empty or not parseable CSV")
		return
	}
	hasHeader := csvLooksLikeHeader(first)
	headers := make([]string, len(first))
	if hasHeader {
		copy(headers, first)
	} else {
		for i := range first {
			headers[i] = fmt.Sprintf("col_%d", i)
		}
	}
	suggestions := suggestCSVMapping(headers)
	if !hasHeader {
		// Headerless file: name-based suggestion is meaningless except for
		// the email column, which we detect from the first data row's '@'.
		for i := range suggestions {
			suggestions[i].Target = "metadata." + headers[i]
			suggestions[i].Confidence = "none"
		}
		for i, c := range first {
			if strings.Contains(c, "@") {
				suggestions[i].Target = "email"
				suggestions[i].Confidence = "medium"
				break
			}
		}
	}
	emailCol := -1
	for _, sug := range suggestions {
		if sug.Target == "email" {
			emailCol = sug.ColumnIndex
			break
		}
	}

	// Stream the WHOLE file once: row_count, invalid emails, per-ISP mix
	// (isp.Group over the mapped email column). Rows are processed one at a
	// time — never buffered beyond the ≤10 sample.
	sample := [][]string{}
	if !hasHeader {
		sample = append(sample, first)
	}
	var rowCount, invalidEmails int64
	ispCounts := map[string]int64{}
	countRow := func(row []string) {
		rowCount++
		if emailCol < 0 {
			return
		}
		var raw string
		if emailCol < len(row) {
			raw = row[emailCol]
		}
		email, ok := csvValidEmail(raw)
		if !ok {
			invalidEmails++
			return
		}
		ispCounts[isppkg.Group(email)]++
	}
	if !hasHeader {
		countRow(first)
	}
	for {
		row, err := up.csv.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("CSV parse error at row %d: %v", rowCount+1, err))
			return
		}
		if len(sample) < partnerCSVSampleRows {
			cp := make([]string, len(row))
			copy(cp, row)
			sample = append(sample, cp)
		}
		countRow(row)
	}

	validEmails := rowCount - invalidEmails
	type ispRow struct {
		ISP   string  `json:"isp"`
		Count int64   `json:"count"`
		Pct   float64 `json:"pct"`
	}
	breakdown := make([]ispRow, 0, len(ispCounts))
	for isp, n := range ispCounts {
		pct := 0.0
		if validEmails > 0 {
			pct = float64(n) * 100 / float64(validEmails)
		}
		breakdown = append(breakdown, ispRow{ISP: isp, Count: n, Pct: pct})
	}
	sort.Slice(breakdown, func(i, j int) bool {
		if breakdown[i].Count != breakdown[j].Count {
			return breakdown[i].Count > breakdown[j].Count
		}
		return breakdown[i].ISP < breakdown[j].ISP
	})

	resp := map[string]interface{}{
		"organization_id":     orgID,
		"dataset_id":          up.dataset,
		"filename":            up.filename,
		"has_header":          hasHeader,
		"headers":             headers,
		"suggested_mapping":   suggestions,
		"sample_rows":         sample,
		"row_count":           rowCount,
		"invalid_email_count": invalidEmails,
		"isp_breakdown":       breakdown,
		"email_column":        emailCol,
	}
	if emailCol < 0 {
		resp["warning"] = "no email column detected — map one before committing; the ISP breakdown is empty"
	}
	respondJSON(w, http.StatusOK, resp)
}

// ── POST /commit ────────────────────────────────────────────────────────────

func (s *PartnerCSVIngestService) HandleCommit(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	if s.s3 == nil {
		respondError(w, http.StatusServiceUnavailable, "ingest pipeline is initialising — retry in a moment")
		return
	}
	up, ok := openCSVUpload(w, r)
	if !ok {
		return
	}
	defer up.closeFn()
	ident, ok := s.resolveDataset(w, r, up.dataset)
	if !ok {
		return
	}

	// mapping: {"<target>": <column_index>} — targets are canonical
	// ingestRecord fields or "metadata.<key>".
	if up.mapping == "" {
		respondError(w, http.StatusBadRequest, "multipart field 'mapping' (JSON {target: column_index}) is required")
		return
	}
	var mapping map[string]int
	if err := json.Unmarshal([]byte(up.mapping), &mapping); err != nil {
		respondError(w, http.StatusBadRequest, "mapping must be a JSON object of {target: column_index}")
		return
	}
	emailCol, hasEmail := mapping["email"]
	if !hasEmail || emailCol < 0 {
		respondError(w, http.StatusBadRequest, "mapping must map the 'email' target to a column")
		return
	}
	mappedCols := map[int]bool{}
	for target, col := range mapping {
		if col < 0 {
			respondError(w, http.StatusBadRequest, "column indexes must be >= 0")
			return
		}
		if target != "email" && csvFieldSetters[target] == nil && !strings.HasPrefix(target, "metadata.") {
			respondError(w, http.StatusBadRequest, "unknown mapping target: "+target)
			return
		}
		mappedCols[col] = true
	}

	first, err := up.csv.Read()
	if err != nil {
		respondError(w, http.StatusBadRequest, "file is empty or not parseable CSV")
		return
	}
	hasHeader := csvLooksLikeHeader(first)
	// metadata keys for UNMAPPED columns: normalized header, or col_<n>.
	metaKeyFor := func(col int) string {
		if hasHeader && col < len(first) {
			if k := normalizeCSVHeader(first[col]); k != "" {
				return k
			}
		}
		return fmt.Sprintf("col_%d", col)
	}
	cell := func(row []string, col int) string {
		if col < len(row) {
			return strings.TrimSpace(row[col])
		}
		return ""
	}
	buildRecord := func(row []string) (ingestRecord, bool) {
		rec := ingestRecord{}
		for target, col := range mapping {
			v := cell(row, col)
			if v == "" {
				continue
			}
			switch {
			case csvFieldSetters[target] != nil:
				csvFieldSetters[target](&rec, v)
			case strings.HasPrefix(target, "metadata."):
				key := strings.TrimPrefix(target, "metadata.")
				if key == "" {
					continue
				}
				if rec.Metadata == nil {
					rec.Metadata = map[string]interface{}{}
				}
				rec.Metadata[key] = v
			}
		}
		for col := range row {
			if mappedCols[col] {
				continue
			}
			v := cell(row, col)
			if v == "" {
				continue
			}
			if rec.Metadata == nil {
				rec.Metadata = map[string]interface{}{}
			}
			key := metaKeyFor(col)
			if _, exists := rec.Metadata[key]; !exists {
				rec.Metadata[key] = v
			}
		}
		return normalizeRecord(rec) // same acceptance test as the API door
	}

	actor := actorFromRequest(r)
	receivedAt := time.Now().UTC()
	var (
		records        int64
		skippedInvalid int64
		batchIDs       []string
		indexDeferred  int
		chunk          = make([]ingestRecord, 0, partnerCSVMaxRecordsPerBatch)
	)
	flush := func() bool {
		if len(chunk) == 0 {
			return true
		}
		batchID := uuid.New().String()
		_, _, deferred, err := persistPartnerBatch(r.Context(), s.db, s.s3, partnerBatchPersistInput{
			BatchID:     batchID,
			PartnerID:   ident.PartnerID,
			PartnerSlug: ident.PartnerSlug,
			DatasetID:   up.dataset,
			DatasetSlug: ident.DatasetSlug,
			Vertical:    ident.Vertical,
			ReceivedAt:  receivedAt,
			ContentType: "text/csv",
			Records:     chunk,
			IngestMeta: map[string]interface{}{
				"source":      "csv_upload",
				"uploaded_by": actor,
				"filename":    up.filename,
			},
		})
		if err != nil {
			// Batches already flushed stay flushed (each is independently
			// slicer-visible); report how far we got rather than pretending
			// nothing happened.
			respondJSON(w, http.StatusBadGateway, map[string]interface{}{
				"error":           "S3 upload failed mid-commit — earlier batches persisted, remainder NOT uploaded; fix and re-upload the remainder only",
				"batch_ids":       batchIDs,
				"records":         records - int64(len(chunk)),
				"skipped_invalid": skippedInvalid,
			})
			return false
		}
		if deferred {
			indexDeferred++
		}
		batchIDs = append(batchIDs, batchID)
		chunk = chunk[:0]
		return true
	}

	process := func(row []string) bool {
		rec, ok := buildRecord(row)
		if !ok {
			skippedInvalid++
			return true
		}
		chunk = append(chunk, rec)
		records++
		if len(chunk) >= partnerCSVMaxRecordsPerBatch {
			return flush()
		}
		return true
	}

	if !hasHeader {
		if !process(first) {
			return
		}
	}
	for {
		row, err := up.csv.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			respondError(w, http.StatusBadRequest, "CSV parse error mid-file: "+err.Error())
			return
		}
		if !process(row) {
			return
		}
	}
	if !flush() {
		return
	}
	if records == 0 {
		respondError(w, http.StatusBadRequest, "no valid records — every row's email column was empty or invalid")
		return
	}

	writeAuditLog(r.Context(), s.db, actor, "csv_upload_commit", "partner_dataset", up.dataset, nil, map[string]interface{}{
		"filename": up.filename, "records": records,
		"skipped_invalid": skippedInvalid, "batches": len(batchIDs),
	})

	respondJSON(w, http.StatusAccepted, map[string]interface{}{
		"organization_id": orgID,
		"dataset_id":      up.dataset,
		"dataset_name":    ident.DatasetName,
		"batch_ids":       batchIDs,
		"batches":         len(batchIDs),
		"records":         records,
		"skipped_invalid": skippedInvalid,
		"index_deferred":  indexDeferred,
		"message":         "batches accepted; the slicer + EO validator process them asynchronously (same path as the partner API)",
	})
}
