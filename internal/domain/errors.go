package domain

import "errors"

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("access denied")
	ErrConflict  = errors.New("state conflict")
	ErrInvalid   = errors.New("invalid input")
)
