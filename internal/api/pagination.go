package api

import (
	"math"
	"net/http"
	"strconv"
)

// PaginationParams holds parsed pagination values from query params.
type PaginationParams struct {
	Page   int
	Limit  int
	Offset int
}

// PaginatedResponse wraps any list data with pagination metadata.
type PaginatedResponse struct {
	Data       interface{}    `json:"data"`
	Pagination PaginationMeta `json:"pagination"`
}

// PaginationMeta contains pagination metadata for the response.
type PaginationMeta struct {
	Page       int   `json:"page"`
	Limit      int   `json:"limit"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"total_pages"`
	HasMore    bool  `json:"has_more"`
}

// ParsePagination extracts page and limit from query params with defaults.
// defaultLimit is used when no limit param is provided.
// maxLimit caps the maximum allowed limit to prevent abuse.
func ParsePagination(r *http.Request, defaultLimit, maxLimit int) PaginationParams {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	return PaginationParams{
		Page:   page,
		Limit:  limit,
		Offset: (page - 1) * limit,
	}
}

// NewPaginatedResponse builds a PaginatedResponse from data, params, and total count.
func NewPaginatedResponse(data interface{}, params PaginationParams, total int64) PaginatedResponse {
	totalPages := int(math.Ceil(float64(total) / float64(params.Limit)))
	if totalPages < 1 {
		totalPages = 1
	}

	return PaginatedResponse{
		Data: data,
		Pagination: PaginationMeta{
			Page:       params.Page,
			Limit:      params.Limit,
			Total:      total,
			TotalPages: totalPages,
			HasMore:    params.Page < totalPages,
		},
	}
}
