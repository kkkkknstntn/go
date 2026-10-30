package httpapi

import (
	"fmt"
	"net/http"

	"awesomeProject/internal/booking"
	"awesomeProject/internal/domain"
)

func (s *server) createOrganizer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Balance int64  `json:"balance"`
	}
	if err := decodeJSON(w, r, &in, false); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	o, err := s.booking.CreateOrganizer(r.Context(), booking.NewOrganizer{ID: in.ID, Name: in.Name, Balance: in.Balance})
	s.respond(w, r, http.StatusCreated, o, err)
}

func (s *server) getOrganizer(w http.ResponseWriter, r *http.Request) {
	o, err := s.booking.GetOrganizer(r.Context(), r.PathValue("id"))
	s.respond(w, r, http.StatusOK, o, err)
}

func (s *server) createEvent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID          string            `json:"id"`
		Name        string            `json:"name"`
		OrganizerID string            `json:"organizerId"`
		Seats       []domain.SeatSpec `json:"seats"`
		Generate    *struct {
			Prefix string `json:"prefix"`
			Count  int    `json:"count"`
			Price  int64  `json:"price"`
		} `json:"generate"`
	}
	if err := decodeJSON(w, r, &in, false); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	seats := in.Seats
	if g := in.Generate; g != nil {
		if g.Count < 1 || g.Count > booking.MaxSeatsPerEvent-len(seats) {
			s.writeError(w, r, fmt.Errorf("%w: generate.count must be in 1..%d", domain.ErrValidation, booking.MaxSeatsPerEvent-len(seats)), nil)
			return
		}
		for i := 1; i <= g.Count; i++ {
			seats = append(seats, domain.SeatSpec{Number: fmt.Sprintf("%s%d", g.Prefix, i), Price: g.Price})
		}
	}
	e, err := s.booking.CreateEvent(r.Context(), booking.NewEvent{ID: in.ID, Name: in.Name, OrganizerID: in.OrganizerID, Seats: seats})
	s.respond(w, r, http.StatusCreated, e, err)
}

func (s *server) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.booking.ListEvents(r.Context())
	s.respond(w, r, http.StatusOK, events, err)
}

func (s *server) listSeats(w http.ResponseWriter, r *http.Request) {
	seats, err := s.booking.ListSeats(r.Context(), r.PathValue("id"))
	s.respond(w, r, http.StatusOK, seats, err)
}
