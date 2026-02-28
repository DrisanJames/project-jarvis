package api

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// TemplateFolderAPI provides REST API handlers for template folders and templates
type TemplateFolderAPI struct {
	db              *sql.DB
	folderService   *mailing.TemplateFolderService
	templateService *mailing.EmailTemplateStore
}

// NewTemplateFolderAPI creates a new template folder API handler
func NewTemplateFolderAPI(db *sql.DB) *TemplateFolderAPI {
	return &TemplateFolderAPI{
		db:              db,
		folderService:   mailing.NewTemplateFolderService(db),
		templateService: mailing.NewEmailTemplateStore(db),
	}
}

// RegisterRoutes registers template folder and template routes
func (api *TemplateFolderAPI) RegisterRoutes(r chi.Router) {
	// Template Folders
	r.Route("/template-folders", func(r chi.Router) {
		r.Get("/", api.HandleListFolders)
		r.Post("/", api.HandleCreateFolder)
		r.Get("/tree", api.HandleListFoldersTree)
		r.Get("/{folderId}", api.HandleGetFolder)
		r.Put("/{folderId}", api.HandleUpdateFolder)
		r.Delete("/{folderId}", api.HandleDeleteFolder)
		r.Post("/{folderId}/move", api.HandleMoveFolder)
		r.Get("/{folderId}/templates", api.HandleListFolderTemplates)
	})

	// Templates with folder support
	r.Route("/templates", func(r chi.Router) {
		r.Get("/", api.HandleListTemplates)
		r.Post("/", api.HandleCreateTemplate)
		r.Get("/search", api.HandleSearchTemplates)
		r.Get("/{templateId}", api.HandleGetTemplate)
		r.Put("/{templateId}", api.HandleUpdateTemplate)
		r.Delete("/{templateId}", api.HandleDeleteTemplate)
		r.Post("/{templateId}/clone", api.HandleCloneTemplate)
		r.Post("/{templateId}/move", api.HandleMoveTemplate)
	})
}

// =============================================================================
// FOLDER HANDLERS
// =============================================================================

// HandleListFolders returns all folders for the organization
func (api *TemplateFolderAPI) HandleListFolders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrganizationUUID(r)

	folders, err := api.folderService.ListFolders(ctx, orgID)
	if err != nil {
		log.Printf("Error listing folders: %v", err)
		http.Error(w, `{"error":"failed to list folders"}`, http.StatusInternalServerError)
		return
	}

	if folders == nil {
		folders = []*mailing.TemplateFolder{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"folders": folders,
		"count":   len(folders),
	})
}

// HandleListFoldersTree returns folders organized in a tree structure
func (api *TemplateFolderAPI) HandleListFoldersTree(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrganizationUUID(r)

	folders, err := api.folderService.ListFoldersTree(ctx, orgID)
	if err != nil {
		log.Printf("Error listing folder tree: %v", err)
		http.Error(w, `{"error":"failed to list folder tree"}`, http.StatusInternalServerError)
		return
	}

	if folders == nil {
		folders = []*mailing.TemplateFolder{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"folders": folders,
		"count":   len(folders),
	})
}

// HandleCreateFolder creates a new template folder
func (api *TemplateFolderAPI) HandleCreateFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrganizationUUID(r)

	var input struct {
		Name     string  `json:"name"`
		ParentID *string `json:"parent_id,omitempty"`
		Path     string  `json:"path,omitempty"` // Alternative: create by path
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	if input.Name == "" && input.Path == "" {
		http.Error(w, `{"error":"name or path is required"}`, http.StatusBadRequest)
		return
	}

	var folder *mailing.TemplateFolder
	var err error

	if input.Path != "" {
		// Create folder by path (creates intermediate folders if needed)
		folder, err = api.folderService.GetOrCreateFolderPath(ctx, orgID, input.Path)
	} else {
		// Create single folder
		var parentID *uuid.UUID
		if input.ParentID != nil && *input.ParentID != "" {
			parsed, parseErr := uuid.Parse(*input.ParentID)
			if parseErr == nil {
				parentID = &parsed
			}
		}
		folder, err = api.folderService.CreateFolder(ctx, orgID, parentID, input.Name)
	}

	if err != nil {
		log.Printf("Error creating folder: %v", err)
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(folder)
}

// HandleGetFolder returns a single folder
func (api *TemplateFolderAPI) HandleGetFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	folderIDStr := chi.URLParam(r, "folderId")
	folderID, err := uuid.Parse(folderIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid folder ID"}`, http.StatusBadRequest)
		return
	}

	folder, err := api.folderService.GetFolder(ctx, folderID)
	if err != nil {
		log.Printf("Error getting folder: %v", err)
		http.Error(w, `{"error":"failed to get folder"}`, http.StatusInternalServerError)
		return
	}

	if folder == nil {
		http.Error(w, `{"error":"folder not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(folder)
}

// HandleUpdateFolder updates a folder's name
func (api *TemplateFolderAPI) HandleUpdateFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	folderIDStr := chi.URLParam(r, "folderId")
	folderID, err := uuid.Parse(folderIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid folder ID"}`, http.StatusBadRequest)
		return
	}

	var input struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	if input.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}

	folder, err := api.folderService.UpdateFolder(ctx, folderID, input.Name)
	if err != nil {
		log.Printf("Error updating folder: %v", err)
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(folder)
}

// HandleDeleteFolder deletes a folder
func (api *TemplateFolderAPI) HandleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	folderIDStr := chi.URLParam(r, "folderId")
	folderID, err := uuid.Parse(folderIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid folder ID"}`, http.StatusBadRequest)
		return
	}

	if err := api.folderService.DeleteFolder(ctx, folderID); err != nil {
		log.Printf("Error deleting folder: %v", err)
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "folder deleted",
		"id":      folderIDStr,
	})
}

// HandleMoveFolder moves a folder to a new parent
func (api *TemplateFolderAPI) HandleMoveFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	folderIDStr := chi.URLParam(r, "folderId")
	folderID, err := uuid.Parse(folderIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid folder ID"}`, http.StatusBadRequest)
		return
	}

	var input struct {
		NewParentID *string `json:"new_parent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	var newParentID *uuid.UUID
	if input.NewParentID != nil && *input.NewParentID != "" {
		parsed, parseErr := uuid.Parse(*input.NewParentID)
		if parseErr == nil {
			newParentID = &parsed
		}
	}

	if err := api.folderService.MoveFolder(ctx, folderID, newParentID); err != nil {
		log.Printf("Error moving folder: %v", err)
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "folder moved",
		"id":      folderIDStr,
	})
}

// HandleListFolderTemplates returns templates in a specific folder
func (api *TemplateFolderAPI) HandleListFolderTemplates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrganizationUUID(r)
	folderIDStr := chi.URLParam(r, "folderId")
	folderID, err := uuid.Parse(folderIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid folder ID"}`, http.StatusBadRequest)
		return
	}

	// Check if recursive
	recursive := r.URL.Query().Get("recursive") == "true"

	var templates []*mailing.EmailTemplate
	if recursive {
		templates, err = api.templateService.ListTemplatesInFolderRecursive(ctx, orgID, folderID)
	} else {
		templates, err = api.templateService.ListTemplates(ctx, orgID, &folderID)
	}

	if err != nil {
		log.Printf("Error listing templates: %v", err)
		http.Error(w, `{"error":"failed to list templates"}`, http.StatusInternalServerError)
		return
	}

	if templates == nil {
		templates = []*mailing.EmailTemplate{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"templates": templates,
		"count":     len(templates),
		"folder_id": folderIDStr,
	})
}

// =============================================================================
// TEMPLATE HANDLERS
// =============================================================================

// HandleListTemplates returns all templates, optionally filtered by folder
func (api *TemplateFolderAPI) HandleListTemplates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrganizationUUID(r)

	var folderID *uuid.UUID
	folderIDStr := r.URL.Query().Get("folder_id")
	if folderIDStr != "" {
		parsed, err := uuid.Parse(folderIDStr)
		if err == nil {
			folderID = &parsed
		}
	}

	templates, err := api.templateService.ListTemplates(ctx, orgID, folderID)
	if err != nil {
		log.Printf("Error listing templates: %v", err)
		http.Error(w, `{"error":"failed to list templates"}`, http.StatusInternalServerError)
		return
	}

	if templates == nil {
		templates = []*mailing.EmailTemplate{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"templates": templates,
		"count":     len(templates),
	})
}

// HandleCreateTemplate creates a new template
func (api *TemplateFolderAPI) HandleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrganizationUUID(r)

	var req mailing.CreateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}

	template, err := api.templateService.CreateTemplate(ctx, orgID, &req)
	if err != nil {
		log.Printf("Error creating template: %v", err)
		http.Error(w, `{"error":"failed to create template"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(template)
}

// HandleGetTemplate returns a single template
func (api *TemplateFolderAPI) HandleGetTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateIDStr := chi.URLParam(r, "templateId")
	templateID, err := uuid.Parse(templateIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid template ID"}`, http.StatusBadRequest)
		return
	}

	template, err := api.templateService.GetTemplate(ctx, templateID)
	if err != nil {
		log.Printf("Error getting template: %v", err)
		http.Error(w, `{"error":"failed to get template"}`, http.StatusInternalServerError)
		return
	}

	if template == nil {
		http.Error(w, `{"error":"template not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(template)
}

// HandleUpdateTemplate updates a template
func (api *TemplateFolderAPI) HandleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateIDStr := chi.URLParam(r, "templateId")
	templateID, err := uuid.Parse(templateIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid template ID"}`, http.StatusBadRequest)
		return
	}

	var req mailing.UpdateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	template, err := api.templateService.UpdateTemplate(ctx, templateID, &req)
	if err != nil {
		log.Printf("Error updating template: %v", err)
		http.Error(w, `{"error":"failed to update template"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(template)
}

// HandleDeleteTemplate deletes a template
func (api *TemplateFolderAPI) HandleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateIDStr := chi.URLParam(r, "templateId")
	templateID, err := uuid.Parse(templateIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid template ID"}`, http.StatusBadRequest)
		return
	}

	if err := api.templateService.DeleteTemplate(ctx, templateID); err != nil {
		log.Printf("Error deleting template: %v", err)
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "template deleted",
		"id":      templateIDStr,
	})
}

// HandleCloneTemplate creates a copy of a template
func (api *TemplateFolderAPI) HandleCloneTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateIDStr := chi.URLParam(r, "templateId")
	templateID, err := uuid.Parse(templateIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid template ID"}`, http.StatusBadRequest)
		return
	}

	var input struct {
		Name     string  `json:"name"`
		FolderID *string `json:"folder_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	if input.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}

	var folderID *uuid.UUID
	if input.FolderID != nil && *input.FolderID != "" {
		parsed, parseErr := uuid.Parse(*input.FolderID)
		if parseErr == nil {
			folderID = &parsed
		}
	}

	template, err := api.templateService.CloneTemplate(ctx, templateID, input.Name, folderID)
	if err != nil {
		log.Printf("Error cloning template: %v", err)
		http.Error(w, `{"error":"failed to clone template"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(template)
}

// HandleMoveTemplate moves a template to a different folder
func (api *TemplateFolderAPI) HandleMoveTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templateIDStr := chi.URLParam(r, "templateId")
	templateID, err := uuid.Parse(templateIDStr)
	if err != nil {
		http.Error(w, `{"error":"invalid template ID"}`, http.StatusBadRequest)
		return
	}

	var input struct {
		FolderID *string `json:"folder_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	var folderID *uuid.UUID
	if input.FolderID != nil && *input.FolderID != "" {
		parsed, parseErr := uuid.Parse(*input.FolderID)
		if parseErr == nil {
			folderID = &parsed
		}
	}

	if err := api.templateService.MoveTemplate(ctx, templateID, folderID); err != nil {
		log.Printf("Error moving template: %v", err)
		http.Error(w, `{"error":"failed to move template"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "template moved",
		"id":      templateIDStr,
	})
}

// HandleSearchTemplates searches templates by name or content
func (api *TemplateFolderAPI) HandleSearchTemplates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrganizationUUID(r)

	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, `{"error":"search query is required"}`, http.StatusBadRequest)
		return
	}

	templates, err := api.templateService.SearchTemplates(ctx, orgID, query)
	if err != nil {
		log.Printf("Error searching templates: %v", err)
		http.Error(w, `{"error":"failed to search templates"}`, http.StatusInternalServerError)
		return
	}

	if templates == nil {
		templates = []*mailing.EmailTemplate{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"templates": templates,
		"count":     len(templates),
		"query":     query,
	})
}
