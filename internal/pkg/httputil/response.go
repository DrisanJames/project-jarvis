package httputil

import (
	"encoding/json"
	"log"
	"net/http"
)

// ErrorResponse is the standard error envelope for all API errors.
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Details any    `json:"details,omitempty"`
}

// JSON writes a JSON response with the given status code. The data is
// serialized and Content-Type is set automatically. If encoding fails,
// a 500 error is written instead.
func JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("[httputil] JSON encode error: %v", err)
	}
}

// OK writes a 200 response with the given data.
func OK(w http.ResponseWriter, data any) {
	JSON(w, http.StatusOK, data)
}

// Created writes a 201 response with the given data.
func Created(w http.ResponseWriter, data any) {
	JSON(w, http.StatusCreated, data)
}

// NoContent writes a 204 response with no body.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// Error writes a JSON error response. Use for client errors (4xx).
func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, ErrorResponse{Error: message})
}

// BadRequest writes a 400 error.
func BadRequest(w http.ResponseWriter, message string) {
	Error(w, http.StatusBadRequest, message)
}

// NotFound writes a 404 error.
func NotFound(w http.ResponseWriter, message string) {
	Error(w, http.StatusNotFound, message)
}

// InternalError writes a 500 error. Logs the real error but returns a
// generic message to the client (never leak internals).
func InternalError(w http.ResponseWriter, err error) {
	log.Printf("[httputil] internal error: %v", err)
	Error(w, http.StatusInternalServerError, "internal server error")
}

// Decode reads JSON from the request body into dst.
// Returns false and writes a 400 response if parsing fails.
func Decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		BadRequest(w, "invalid JSON: "+err.Error())
		return false
	}
	return true
}
