package campaign

import "errors"

// Sentinel errors for the campaign service layer.
var (
	ErrNotFound          = errors.New("campaign not found")
	ErrInvalidTransition = errors.New("invalid status transition")
	ErrMissingList       = errors.New("campaign has no list or segment")
	ErrAlreadySending    = errors.New("campaign is already sending or sent")
)
