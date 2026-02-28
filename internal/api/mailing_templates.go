package api

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (s *AdvancedMailingService) HandleGetTemplates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := s.db.QueryContext(ctx, `
		SELECT id, name, description, subject, thumbnail_url, status, created_at
		FROM mailing_templates ORDER BY created_at DESC
	`)
	defer rows.Close()
	
	var templates []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name, status string
		var desc, subject, thumbnail *string
		var createdAt time.Time
		rows.Scan(&id, &name, &desc, &subject, &thumbnail, &status, &createdAt)
		templates = append(templates, map[string]interface{}{
			"id": id.String(), "name": name, "description": desc, "subject": subject,
			"thumbnail_url": thumbnail, "status": status, "created_at": createdAt,
		})
	}
	if templates == nil { templates = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"templates": templates})
}

func (s *AdvancedMailingService) HandleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		CategoryID   string `json:"category_id"`
		Subject      string `json:"subject"`
		FromName     string `json:"from_name"`
		FromEmail    string `json:"from_email"`
		HTMLContent  string `json:"html_content"`
		PlainContent string `json:"plain_content"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	templateID := uuid.New()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	
	var categoryID *uuid.UUID
	if input.CategoryID != "" {
		cid, _ := uuid.Parse(input.CategoryID)
		categoryID = &cid
	}
	
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_templates (id, organization_id, category_id, name, description, subject, from_name, from_email, html_content, plain_content, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'active', NOW(), NOW())
	`, templateID, orgID, categoryID, input.Name, input.Description, input.Subject, input.FromName, input.FromEmail, input.HTMLContent, input.PlainContent)
	
	if err != nil {
		log.Printf("Error creating template: %v", err)
		http.Error(w, `{"error":"failed to create template"}`, http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"id": templateID.String(), "name": input.Name})
}

func (s *AdvancedMailingService) HandleGetTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateID := chi.URLParam(r, "templateId")
	
	var id uuid.UUID
	var name string
	var desc, subject, fromName, fromEmail, htmlContent, plainContent *string
	
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, description, subject, from_name, from_email, html_content, plain_content
		FROM mailing_templates WHERE id = $1
	`, templateID).Scan(&id, &name, &desc, &subject, &fromName, &fromEmail, &htmlContent, &plainContent)
	
	if err != nil {
		http.Error(w, `{"error":"template not found"}`, http.StatusNotFound)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id.String(), "name": name, "description": desc,
		"subject": subject, "from_name": fromName, "from_email": fromEmail,
		"html_content": htmlContent, "plain_content": plainContent,
	})
}

func (s *AdvancedMailingService) HandleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateID := chi.URLParam(r, "templateId")
	
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Subject     string `json:"subject"`
		HTMLContent string `json:"html_content"`
		PlainContent string `json:"plain_content"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	s.db.ExecContext(ctx, `
		UPDATE mailing_templates SET 
			name = COALESCE(NULLIF($2, ''), name),
			description = COALESCE(NULLIF($3, ''), description),
			subject = COALESCE(NULLIF($4, ''), subject),
			html_content = COALESCE(NULLIF($5, ''), html_content),
			plain_content = COALESCE(NULLIF($6, ''), plain_content),
			updated_at = NOW()
		WHERE id = $1
	`, templateID, input.Name, input.Description, input.Subject, input.HTMLContent, input.PlainContent)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": templateID, "updated": true})
}

func (s *AdvancedMailingService) HandleCloneTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateID := chi.URLParam(r, "templateId")
	
	newID := uuid.New()
	s.db.ExecContext(ctx, `
		INSERT INTO mailing_templates (id, organization_id, name, description, category, subject, html_content, plain_content, status, created_at, updated_at)
		SELECT $2, organization_id, name || ' (Copy)', description, category, subject, html_content, plain_content, 'active', NOW(), NOW()
		FROM mailing_templates WHERE id = $1
	`, templateID, newID)
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"id": newID.String(), "cloned_from": templateID})
}

func (s *AdvancedMailingService) HandleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateID := chi.URLParam(r, "templateId")
	
	s.db.ExecContext(ctx, `DELETE FROM mailing_templates WHERE id = $1`, templateID)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"deleted": templateID})
}

// ================== TAGS ==================

func (s *AdvancedMailingService) HandleGetTags(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := s.db.QueryContext(ctx, `
		SELECT t.id, t.name, t.color, COUNT(ta.subscriber_id) as subscriber_count
		FROM mailing_subscriber_tags t
		LEFT JOIN mailing_subscriber_tag_assignments ta ON ta.tag_id = t.id
		GROUP BY t.id ORDER BY t.name
	`)
	defer rows.Close()
	
	var tags []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name, color string
		var count int
		rows.Scan(&id, &name, &color, &count)
		tags = append(tags, map[string]interface{}{
			"id": id.String(), "name": name, "color": color, "subscriber_count": count,
		})
	}
	if tags == nil { tags = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"tags": tags})
}

func (s *AdvancedMailingService) HandleCreateTag(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	tagID := uuid.New()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	
	if input.Color == "" { input.Color = "#667eea" }
	
	s.db.ExecContext(ctx, `
		INSERT INTO mailing_subscriber_tags (id, organization_id, name, color, created_at)
		VALUES ($1, $2, $3, $4, NOW())
	`, tagID, orgID, input.Name, input.Color)
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"id": tagID.String(), "name": input.Name, "color": input.Color})
}

func (s *AdvancedMailingService) HandleAssignTags(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subscriberID := chi.URLParam(r, "subscriberId")
	
	var input struct {
		TagIDs []string `json:"tag_ids"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	for _, tagID := range input.TagIDs {
		s.db.ExecContext(ctx, `
			INSERT INTO mailing_subscriber_tag_assignments (subscriber_id, tag_id, assigned_at)
			VALUES ($1, $2, NOW()) ON CONFLICT DO NOTHING
		`, subscriberID, tagID)
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"assigned": len(input.TagIDs)})
}
