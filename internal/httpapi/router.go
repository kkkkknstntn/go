// Package httpapi is the HTTP/JSON API.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"awesomeProject/internal/auth"
	"awesomeProject/internal/booking"
	"awesomeProject/internal/domain"
)

type Deps struct {
	Booking        *booking.Service
	Auth           *auth.Service
	Logger         *slog.Logger
	Ready          func(ctx context.Context) error // checked by /readyz
	RequestTimeout time.Duration                   // 0: no limit
}

type server struct {
	booking *booking.Service
	auth    *auth.Service
	log     *slog.Logger
	ready   func(ctx context.Context) error
}

type access struct {
	public bool          // no token needed
	roles  []domain.Role // empty: any authenticated caller
}

var (
	public        = access{public: true}
	authenticated = access{}
	buyer         = access{roles: []domain.Role{domain.RoleUser}}
	admin         = access{roles: []domain.Role{domain.RoleAdmin}}
	// admins may call payment callbacks by hand for demonstrations
	paymentSystem = access{roles: []domain.Role{domain.RoleProvider, domain.RoleAdmin}}
)

type route struct {
	method, pattern string
	access          access
	handler         http.HandlerFunc
}

func (s *server) routes() []route {
	return []route{
		{"GET", "/healthz", public, s.health},
		{"GET", "/readyz", public, s.readiness},
		{"GET", "/openapi.json", public, s.openAPI},
		{"GET", "/docs", public, s.swaggerUI},

		{"POST", "/users", public, s.register},
		{"GET", "/users/me", authenticated, s.me},
		{"GET", "/users/{id}", authenticated, s.getUser},
		{"POST", "/auth/login", public, s.login},
		{"POST", "/auth/refresh", public, s.refresh},
		{"POST", "/auth/logout", public, s.logout},

		{"POST", "/organizers", admin, s.createOrganizer},
		{"GET", "/organizers/{id}", admin, s.getOrganizer},
		{"GET", "/events", public, s.listEvents},
		{"POST", "/events", admin, s.createEvent},
		{"GET", "/events/{id}/seats", public, s.listSeats},

		{"POST", "/reservations", buyer, s.reserve},
		{"GET", "/reservations/{id}", authenticated, s.getReservation},
		{"GET", "/reservations/{id}/history", authenticated, s.reservationHistory},
		{"POST", "/reservations/{id}/cancel", buyer, s.cancelReservation},
		{"POST", "/reservations/{id}/purchase", buyer, s.startPurchase},

		{"GET", "/purchases/{id}", authenticated, s.getPurchase},
		{"GET", "/payments/{id}", authenticated, s.getPayment},
		{"POST", "/payments/{id}/confirm", paymentSystem, s.confirmPayment},
		{"POST", "/payments/{id}/fail", paymentSystem, s.failPayment},
	}
}

func NewRouter(d Deps) http.Handler {
	s := &server{booking: d.Booking, auth: d.Auth, log: d.Logger, ready: d.Ready}
	if s.log == nil {
		s.log = slog.Default()
	}
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.Handle(rt.method+" "+rt.pattern, s.authorize(rt.access, rt.handler))
	}
	// recoverPanics is inside accessLog so that a panic is logged as a 500.
	return withRequestID(s.accessLog(s.recoverPanics(withTimeout(d.RequestTimeout, mux))))
}
