package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/postgres"
)

const maxBodyBytes = 1 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, optional bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var tooLarge *http.MaxBytesError
	if err := dec.Decode(dst); err != nil {
		switch {
		case errors.As(err, &tooLarge):
			return err
		case optional && errors.Is(err, io.EOF):
			return nil
		}
		return fmt.Errorf("%w: invalid JSON body: %v", domain.ErrValidation, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if errors.As(err, &tooLarge) {
			return err
		}
		return fmt.Errorf("%w: body must contain exactly one JSON object", domain.ErrValidation)
	}
	return nil
}

type errorBody struct {
	Error   string          `json:"error"`
	Message string          `json:"message"`
	Payment *domain.Payment `json:"payment,omitempty"`
}

var errorStatuses = []struct {
	err    error
	status int
	code   string
}{
	{domain.ErrValidation, http.StatusBadRequest, "validation_failed"},
	{domain.ErrUnauthenticated, http.StatusUnauthorized, "unauthenticated"},
	{domain.ErrInvalidToken, http.StatusUnauthorized, "invalid_token"},
	{domain.ErrInvalidCredentials, http.StatusUnauthorized, "invalid_credentials"},
	{domain.ErrForbidden, http.StatusForbidden, "forbidden"},
	{domain.ErrNotFound, http.StatusNotFound, "not_found"},
	{domain.ErrAlreadyExists, http.StatusConflict, "already_exists"},
	{domain.ErrSeatUnavailable, http.StatusConflict, "seat_unavailable"},
	{domain.ErrReservationExpired, http.StatusConflict, "reservation_expired"},
	{domain.ErrPaymentTimedOut, http.StatusConflict, "payment_timed_out"},
	{domain.ErrPaymentNotPending, http.StatusConflict, "payment_not_pending"},
	{domain.ErrInvalidTransition, http.StatusConflict, "invalid_transition"},
	{domain.ErrInsufficientFunds, http.StatusPaymentRequired, "insufficient_funds"},
}

func (s *server) writeError(w http.ResponseWriter, r *http.Request, err error, payment *domain.Payment) {
	for _, m := range errorStatuses {
		if errors.Is(err, m.err) {
			if m.status == http.StatusUnauthorized && !errors.Is(err, domain.ErrInvalidCredentials) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="last-ticket"`)
			}
			writeJSON(w, m.status, errorBody{Error: m.code, Message: err.Error(), Payment: payment})
			return
		}
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: "body_too_large", Message: err.Error()})
	case errors.Is(err, context.DeadlineExceeded):
		s.log.WarnContext(r.Context(), "request timed out", "path", r.URL.Path, "request_id", requestID(r.Context()))
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "timeout", Message: "request took too long, nothing was changed"})
	case errors.Is(err, context.Canceled):
		s.log.InfoContext(r.Context(), "request cancelled", "path", r.URL.Path, "request_id", requestID(r.Context()))
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "request_cancelled", Message: "request was cancelled, nothing was changed"})
	case postgres.IsTransient(err):
		s.log.WarnContext(r.Context(), "transient database error", "err", err, "request_id", requestID(r.Context()))
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "service_unavailable", Message: "temporary database problem, nothing was changed; retry later"})
	default:
		s.log.ErrorContext(r.Context(), "internal error", "err", err,
			"method", r.Method, "path", r.URL.Path, "request_id", requestID(r.Context()))
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal", Message: "internal error"})
	}
}

func (s *server) respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, status, v)
}
