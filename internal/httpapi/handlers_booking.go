package httpapi

import (
	"fmt"
	"net/http"

	"awesomeProject/internal/domain"
)

func callerID(r *http.Request, bodyUserID string) (string, error) {
	id := principal(r).UserID
	if bodyUserID != "" && bodyUserID != id {
		return "", fmt.Errorf("%w: userId must be your own id", domain.ErrForbidden)
	}
	return id, nil
}

func (s *server) reserve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserID  string `json:"userId"`
		EventID string `json:"eventId"`
		SeatID  string `json:"seatId"`
	}
	if err := decodeJSON(w, r, &in, false); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	userID, err := callerID(r, in.UserID)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	res, err := s.booking.Reserve(r.Context(), userID, in.EventID, in.SeatID)
	s.respond(w, r, http.StatusCreated, res, err)
}

func (s *server) getReservation(w http.ResponseWriter, r *http.Request) {
	res, err := s.booking.GetReservation(r.Context(), r.PathValue("id"))
	if err == nil && !principal(r).CanAccess(res.UserID) {
		err = fmt.Errorf("%w: reservation belongs to another user", domain.ErrForbidden)
	}
	s.respond(w, r, http.StatusOK, res, err)
}

func (s *server) reservationHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := s.booking.GetReservation(r.Context(), id)
	if err == nil && !principal(r).CanAccess(res.UserID) {
		err = fmt.Errorf("%w: reservation belongs to another user", domain.ErrForbidden)
	}
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	history, err := s.booking.ReservationHistory(r.Context(), id)
	s.respond(w, r, http.StatusOK, history, err)
}

func (s *server) cancelReservation(w http.ResponseWriter, r *http.Request) {
	res, err := s.booking.CancelReservation(r.Context(), r.PathValue("id"), principal(r).UserID)
	s.respond(w, r, http.StatusOK, res, err)
}

func (s *server) startPurchase(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserID string `json:"userId"`
	}
	if err := decodeJSON(w, r, &in, true); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	userID, err := callerID(r, in.UserID)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	p, err := s.booking.StartPurchase(r.Context(), r.PathValue("id"), userID)
	s.respond(w, r, http.StatusCreated, p, err)
}

func (s *server) getPurchase(w http.ResponseWriter, r *http.Request) {
	p, err := s.booking.GetPurchase(r.Context(), r.PathValue("id"))
	if err == nil && !principal(r).CanAccess(p.UserID) {
		err = fmt.Errorf("%w: purchase belongs to another user", domain.ErrForbidden)
	}
	s.respond(w, r, http.StatusOK, p, err)
}

func (s *server) getPayment(w http.ResponseWriter, r *http.Request) {
	p, err := s.booking.GetPayment(r.Context(), r.PathValue("id"))
	if err == nil {
		caller := principal(r)
		if !caller.CanAccess(p.UserID) && !caller.HasRole(domain.RoleProvider) {
			err = fmt.Errorf("%w: payment belongs to another user", domain.ErrForbidden)
		}
	}
	s.respond(w, r, http.StatusOK, p, err)
}

func (s *server) confirmPayment(w http.ResponseWriter, r *http.Request) {
	p, err := s.booking.ConfirmPayment(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err, p)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) failPayment(w http.ResponseWriter, r *http.Request) {
	p, err := s.booking.FailPayment(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err, p)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
