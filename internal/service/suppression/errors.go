package suppression

import "errors"

// Sentinel errors for the suppression service layer.
var (
	ErrNotFound = errors.New("suppression entry not found")
)
