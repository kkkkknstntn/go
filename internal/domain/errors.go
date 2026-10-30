package domain

import "errors"

var (
	ErrValidation         = errors.New("validation failed")
	ErrNotFound           = errors.New("not found")
	ErrAlreadyExists      = errors.New("already exists")
	ErrUnauthenticated    = errors.New("authentication required")
	ErrInvalidCredentials = errors.New("invalid login or password")
	ErrInvalidToken       = errors.New("invalid or expired token")
	ErrForbidden          = errors.New("access denied")
	ErrSeatUnavailable    = errors.New("seat is not available")
	ErrInvalidTransition  = errors.New("invalid state transition")
	ErrReservationExpired = errors.New("reservation expired")
	ErrPaymentTimedOut    = errors.New("payment timed out")
	ErrPaymentNotPending  = errors.New("payment is not pending")
	ErrInsufficientFunds  = errors.New("insufficient funds")
)
