package httpapi

import (
	"fmt"
	"net/http"

	"awesomeProject/internal/auth"
	"awesomeProject/internal/domain"
)

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		Login    string `json:"login"`
		Password string `json:"password"`
		Balance  int64  `json:"balance"`
	}
	if err := decodeJSON(w, r, &in, false); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	u, err := s.auth.Register(r.Context(), auth.Registration{Name: in.Name, Login: in.Login, Password: in.Password, Balance: in.Balance})
	s.respond(w, r, http.StatusCreated, u, err)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &in, false); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	pair, err := s.auth.Login(r.Context(), in.Login, in.Password)
	s.respond(w, r, http.StatusOK, pair, err)
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

func (s *server) refresh(w http.ResponseWriter, r *http.Request) {
	var in refreshRequest
	if err := decodeJSON(w, r, &in, false); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	pair, err := s.auth.Refresh(r.Context(), in.RefreshToken)
	s.respond(w, r, http.StatusOK, pair, err)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	var in refreshRequest
	if err := decodeJSON(w, r, &in, false); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	if err := s.auth.Logout(r.Context(), in.RefreshToken); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	u, err := s.booking.GetUser(r.Context(), principal(r).UserID)
	s.respond(w, r, http.StatusOK, u, err)
}

func (s *server) getUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !principal(r).CanAccess(id) {
		s.writeError(w, r, fmt.Errorf("%w: you can only see your own account", domain.ErrForbidden), nil)
		return
	}
	u, err := s.booking.GetUser(r.Context(), id)
	s.respond(w, r, http.StatusOK, u, err)
}
