// Package httpapi exposes the service over HTTP/JSON.
package httpapi

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"awesomeProject/internal/service"
)

//go:embed openapi.json
var openapiSpec []byte

const swaggerPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Last Ticket API</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui.css"></head>
<body><div id="ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url: "/openapi.json", dom_id: "#ui"});</script>
</body></html>`

type Server struct {
	svc *service.Service
}

func New(svc *service.Service) http.Handler {
	s := &Server{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(openapiSpec)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, swaggerPage)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })

	mux.HandleFunc("POST /users", s.createUser)
	mux.HandleFunc("GET /users/{id}", s.getUser)
	mux.HandleFunc("POST /organizers", s.createOrganizer)
	mux.HandleFunc("GET /organizers/{id}", s.getOrganizer)
	mux.HandleFunc("POST /events", s.createEvent)
	mux.HandleFunc("GET /events", s.listEvents)
	mux.HandleFunc("GET /events/{id}/seats", s.listSeats)

	mux.HandleFunc("POST /reservations", s.reserve)
	mux.HandleFunc("GET /reservations/{id}", s.getReservation)
	mux.HandleFunc("POST /reservations/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /reservations/{id}/purchase", s.purchase)

	mux.HandleFunc("GET /purchases/{id}", s.getPurchase)
	mux.HandleFunc("GET /payments/{id}", s.getPayment)
	mux.HandleFunc("POST /payments/{id}/confirm", s.confirm)
	mux.HandleFunc("POST /payments/{id}/fail", s.fail)
	mux.HandleFunc("GET /history/{id}", s.history)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type errBody struct {
	Error   string           `json:"error"`
	Message string           `json:"message"`
	Payment *service.Payment `json:"payment,omitempty"`
}

var errStatus = []struct {
	err    error
	status int
	code   string
}{
	{service.ErrBadRequest, http.StatusBadRequest, "bad_request"},
	{service.ErrNotFound, http.StatusNotFound, "not_found"},
	{service.ErrForbidden, http.StatusForbidden, "forbidden"},
	{service.ErrAlreadyExists, http.StatusConflict, "already_exists"},
	{service.ErrSeatUnavailable, http.StatusConflict, "seat_unavailable"},
	{service.ErrReservationExpired, http.StatusConflict, "reservation_expired"},
	{service.ErrPaymentTimedOut, http.StatusConflict, "payment_timed_out"},
	{service.ErrPaymentNotPending, http.StatusConflict, "payment_not_pending"},
	{service.ErrInvalidState, http.StatusConflict, "invalid_state"},
	{service.ErrInsufficientFunds, http.StatusPaymentRequired, "insufficient_funds"},
}

// writeErr maps domain errors to HTTP statuses. A non-nil payment is attached
// when the failure outcome was persisted (e.g. insufficient_funds, timed_out).
func writeErr(w http.ResponseWriter, err error, p *service.Payment) {
	for _, e := range errStatus {
		if errors.Is(err, e.err) {
			writeJSON(w, e.status, errBody{Error: e.code, Message: err.Error(), Payment: p})
			return
		}
	}
	writeJSON(w, http.StatusInternalServerError, errBody{Error: "internal", Message: "internal error"})
	fmt.Printf("internal error: %v\n", err)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, fmt.Errorf("%w: invalid JSON body: %v", service.ErrBadRequest, err), nil)
		return false
	}
	return true
}

func reply[T any](w http.ResponseWriter, status int, v T, err error) {
	if err != nil {
		writeErr(w, err, nil)
		return
	}
	writeJSON(w, status, v)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Balance int64  `json:"balance"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, err := s.svc.CreateUser(r.Context(), in.ID, in.Name, in.Balance)
	reply(w, http.StatusCreated, u, err)
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.svc.GetUser(r.Context(), r.PathValue("id"))
	reply(w, http.StatusOK, u, err)
}

func (s *Server) createOrganizer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Balance int64  `json:"balance"`
	}
	if !decode(w, r, &in) {
		return
	}
	o, err := s.svc.CreateOrganizer(r.Context(), in.ID, in.Name, in.Balance)
	reply(w, http.StatusCreated, o, err)
}

func (s *Server) getOrganizer(w http.ResponseWriter, r *http.Request) {
	o, err := s.svc.GetOrganizer(r.Context(), r.PathValue("id"))
	reply(w, http.StatusOK, o, err)
}

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID          string             `json:"id"`
		Name        string             `json:"name"`
		OrganizerID string             `json:"organizerId"`
		Seats       []service.SeatSpec `json:"seats"`
		Generate    *struct {
			Prefix string `json:"prefix"`
			Count  int    `json:"count"`
			Price  int64  `json:"price"`
		} `json:"generate"`
	}
	if !decode(w, r, &in) {
		return
	}
	seats := in.Seats
	if g := in.Generate; g != nil {
		if g.Count <= 0 || g.Count > 10000 {
			writeErr(w, fmt.Errorf("%w: generate.count must be in 1..10000", service.ErrBadRequest), nil)
			return
		}
		for i := 1; i <= g.Count; i++ {
			seats = append(seats, service.SeatSpec{Number: fmt.Sprintf("%s%d", g.Prefix, i), Price: g.Price})
		}
	}
	e, err := s.svc.CreateEvent(r.Context(), in.ID, in.Name, in.OrganizerID, seats)
	reply(w, http.StatusCreated, e, err)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	e, err := s.svc.ListEvents(r.Context())
	reply(w, http.StatusOK, e, err)
}

func (s *Server) listSeats(w http.ResponseWriter, r *http.Request) {
	seats, err := s.svc.ListSeats(r.Context(), r.PathValue("id"))
	reply(w, http.StatusOK, seats, err)
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserID  string `json:"userId"`
		EventID string `json:"eventId"`
		SeatID  string `json:"seatId"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := s.svc.Reserve(r.Context(), in.UserID, in.EventID, in.SeatID)
	reply(w, http.StatusCreated, res, err)
}

func (s *Server) getReservation(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.GetReservation(r.Context(), r.PathValue("id"))
	reply(w, http.StatusOK, res, err)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Cancel(r.Context(), r.PathValue("id"))
	reply(w, http.StatusOK, res, err)
}

func (s *Server) purchase(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserID string `json:"userId"`
	}
	if !decode(w, r, &in) {
		return
	}
	p, err := s.svc.StartPurchase(r.Context(), r.PathValue("id"), in.UserID)
	reply(w, http.StatusCreated, p, err)
}

func (s *Server) getPurchase(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.GetPurchase(r.Context(), r.PathValue("id"))
	reply(w, http.StatusOK, p, err)
}

func (s *Server) getPayment(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.GetPayment(r.Context(), r.PathValue("id"))
	reply(w, http.StatusOK, p, err)
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.Confirm(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err, p)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.Fail(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err, p)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	t, err := s.svc.Transitions(r.Context(), r.PathValue("id"))
	reply(w, http.StatusOK, t, err)
}
