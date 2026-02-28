// Package httputil provides shared HTTP response/request utilities for handlers.
//
// Every handler file should use these helpers instead of writing raw
// http.ResponseWriter calls. This ensures consistent JSON formatting,
// error structures, and logging across all endpoints.
package httputil
