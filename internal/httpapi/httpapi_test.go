package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"awesomeProject/internal/auth"
	"awesomeProject/internal/booking"
	"awesomeProject/internal/domain"
	"awesomeProject/internal/ids"
	"awesomeProject/internal/testutil/pgtest"
)

var ctx = context.Background()

type harness struct {
	t       *testing.T
	handler http.Handler
	auth    *auth.Service
	tokens  *auth.TokenManager
	booking *booking.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithTimeout(t, 0)
}

func newHarnessWithTimeout(t *testing.T, requestTimeout time.Duration) *harness {
	t.Helper()
	pool := pgtest.Pool(t)
	bookingSvc, err := booking.New(pool, booking.Config{ReservationTTL: time.Minute, PaymentTimeout: time.Minute})
	must(t, err)
	tokens, err := auth.NewTokenManager([]byte("0123456789abcdef0123456789abcdef"), "test", 15*time.Minute)
	must(t, err)
	authSvc, err := auth.NewService(pool, tokens, auth.Config{RefreshTTL: time.Hour, BcryptCost: bcrypt.MinCost})
	must(t, err)
	handler := NewRouter(Deps{
		Booking:        bookingSvc,
		Auth:           authSvc,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Ready:          pool.Ping,
		RequestTimeout: requestTimeout,
	})
	return &harness{t: t, handler: handler, auth: authSvc, tokens: tokens, booking: bookingSvc}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T, out any) {
	t.Helper()
	if err := json.Unmarshal(r.body, out); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
}

func (r response) errorCode(t *testing.T) string {
	t.Helper()
	var e errorBody
	r.json(t, &e)
	return e.Error
}

func (h *harness) call(method, path, token string, body any) response {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		must(h.t, err)
		rd = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return response{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

func (h *harness) expect(method, path, token string, body any, status int) response {
	h.t.Helper()
	r := h.call(method, path, token, body)
	if r.status != status {
		h.t.Fatalf("%s %s: status %d, want %d; body %s", method, path, r.status, status, r.body)
	}
	return r
}

func (h *harness) buyer(balance int64) (string, string) {
	h.t.Helper()
	u, err := h.auth.Register(ctx, auth.Registration{Name: "buyer", Login: ids.New("b-"), Password: "password-1", Balance: balance})
	must(h.t, err)
	return u.ID, h.token(u.ID, domain.RoleUser)
}

func (h *harness) token(userID string, role domain.Role) string {
	h.t.Helper()
	tok, err := h.tokens.Issue(auth.Principal{UserID: userID, Role: role})
	must(h.t, err)
	return tok
}

func (h *harness) staff(role domain.Role) string {
	h.t.Helper()
	login := ids.New("staff-")
	_, err := h.auth.EnsureAccount(ctx, "staff", login, "password-1", role)
	must(h.t, err)
	pair, err := h.auth.Login(ctx, login, "password-1")
	must(h.t, err)
	return pair.AccessToken
}

func (h *harness) event(orgBalance int64, prices ...int64) (eventID, orgID string) {
	h.t.Helper()
	org, err := h.booking.CreateOrganizer(ctx, booking.NewOrganizer{Name: "org", Balance: orgBalance})
	must(h.t, err)
	seats := make([]domain.SeatSpec, len(prices))
	for i, p := range prices {
		seats[i] = domain.SeatSpec{Number: fmt.Sprintf("A%d", i+1), Price: p}
	}
	ev, err := h.booking.CreateEvent(ctx, booking.NewEvent{Name: "e", OrganizerID: org.ID, Seats: seats})
	must(h.t, err)
	return ev.ID, org.ID
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestFullFlow(t *testing.T) {
	h := newHarness(t)
	adminTok, providerTok := h.staff(domain.RoleAdmin), h.staff(domain.RoleProvider)

	login := ids.New("ivan-")
	var user domain.User
	h.expect("POST", "/users", "", map[string]any{"name": "Ivan", "login": login, "password": "secret-pass", "balance": 10000}, 201).json(t, &user)
	if user.Balance != 10000 || user.Role != domain.RoleUser {
		t.Fatalf("user: %+v", user)
	}
	var pair auth.TokenPair
	h.expect("POST", "/auth/login", "", map[string]any{"login": login, "password": "secret-pass"}, 200).json(t, &pair)
	tok := pair.AccessToken

	var org domain.Organizer
	h.expect("POST", "/organizers", adminTok, map[string]any{"name": "Concert Company", "balance": 50000}, 201).json(t, &org)
	var ev domain.Event
	h.expect("POST", "/events", adminTok, map[string]any{"name": "Concert", "organizerId": org.ID,
		"generate": map[string]any{"prefix": "A", "count": 20, "price": 2000}}, 201).json(t, &ev)

	var seats []domain.Seat
	h.expect("GET", "/events/"+ev.ID+"/seats", "", nil, 200).json(t, &seats)
	if len(seats) != 20 || seats[16].Number != "A17" || seats[16].Status != domain.SeatAvailable {
		t.Fatalf("seats: %+v", seats)
	}

	var res domain.Reservation
	h.expect("POST", "/reservations", tok, map[string]any{"eventId": ev.ID, "seatId": "A17"}, 201).json(t, &res)
	if res.Status != domain.ReservationReserved || res.UserID != user.ID {
		t.Fatalf("reservation: %+v", res)
	}
	_, other := h.buyer(10000)
	if code := h.expect("POST", "/reservations", other, map[string]any{"eventId": ev.ID, "seatId": "A17"}, 409).errorCode(t); code != "seat_unavailable" {
		t.Fatalf("error code %s", code)
	}

	var started domain.PurchaseStarted
	h.expect("POST", "/reservations/"+res.ID+"/purchase", tok, nil, 201).json(t, &started)
	if started.Amount != 2000 || started.Status != domain.PurchasePaymentPending {
		t.Fatalf("purchase: %+v", started)
	}

	h.expect("POST", "/payments/"+started.PaymentID+"/confirm", tok, nil, 403)
	for range 2 {
		var pay domain.Payment
		h.expect("POST", "/payments/"+started.PaymentID+"/confirm", providerTok, nil, 200).json(t, &pay)
		if pay.Status != domain.PaymentSucceeded || pay.PurchaseStatus != domain.PurchasePurchased {
			t.Fatalf("payment: %+v", pay)
		}
	}

	var me domain.User
	h.expect("GET", "/users/me", tok, nil, 200).json(t, &me)
	var orgAfter domain.Organizer
	h.expect("GET", "/organizers/"+org.ID, adminTok, nil, 200).json(t, &orgAfter)
	if me.Balance != 8000 || orgAfter.Balance != 52000 {
		t.Fatalf("balances: user %d, organizer %d", me.Balance, orgAfter.Balance)
	}

	var purchase domain.Purchase
	h.expect("GET", "/purchases/"+started.PurchaseID, tok, nil, 200).json(t, &purchase)
	if purchase.Status != domain.PurchasePurchased {
		t.Fatalf("purchase: %+v", purchase)
	}
	h.expect("POST", "/reservations/"+res.ID+"/cancel", tok, nil, 409)

	var history []domain.Transition
	h.expect("GET", "/reservations/"+res.ID+"/history", tok, nil, 200).json(t, &history)
	if len(history) != 7 || history[len(history)-1].To != "purchased" {
		t.Fatalf("history: %+v", history)
	}
}

func TestAuthenticationAndRoles(t *testing.T) {
	h := newHarness(t)
	_, userTok := h.buyer(0)
	providerTok := h.staff(domain.RoleProvider)

	for _, tok := range []string{"", "garbage", h.token("x", domain.RoleUser) + "x"} {
		r := h.call("GET", "/users/me", tok, nil)
		if r.status != 401 || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
			t.Fatalf("token %q: status %d, header %q", tok, r.status, r.header.Get("WWW-Authenticate"))
		}
	}
	r := h.call("GET", "/users/me", "", nil)
	if code := r.errorCode(t); code != "unauthenticated" {
		t.Fatalf("code %s", code)
	}

	h.expect("POST", "/organizers", userTok, map[string]any{"name": "x", "balance": 0}, 403)
	h.expect("POST", "/events", providerTok, map[string]any{"name": "x", "organizerId": "x"}, 403)
	h.expect("POST", "/reservations", providerTok, map[string]any{"eventId": "x", "seatId": "A1"}, 403)
	h.expect("GET", "/organizers/x", userTok, nil, 403)

	h.expect("GET", "/events", "", nil, 200)
	h.expect("GET", "/healthz", "", nil, 200)
	h.expect("GET", "/readyz", "", nil, 200)
	h.expect("GET", "/openapi.json", "", nil, 200)
}

func TestOwnership(t *testing.T) {
	h := newHarness(t)
	ev, _ := h.event(0, 2000)
	aliceID, alice := h.buyer(10000)
	_, bob := h.buyer(10000)
	adminTok, providerTok := h.staff(domain.RoleAdmin), h.staff(domain.RoleProvider)

	var res domain.Reservation
	h.expect("POST", "/reservations", alice, map[string]any{"eventId": ev, "seatId": "A1"}, 201).json(t, &res)
	h.expect("GET", "/reservations/"+res.ID, bob, nil, 403)
	h.expect("GET", "/reservations/"+res.ID+"/history", bob, nil, 403)
	h.expect("POST", "/reservations/"+res.ID+"/cancel", bob, nil, 403)
	h.expect("POST", "/reservations/"+res.ID+"/purchase", bob, nil, 403)
	h.expect("GET", "/users/"+aliceID, bob, nil, 403)

	var started domain.PurchaseStarted
	h.expect("POST", "/reservations/"+res.ID+"/purchase", alice, nil, 201).json(t, &started)
	h.expect("GET", "/purchases/"+started.PurchaseID, bob, nil, 403)
	h.expect("GET", "/payments/"+started.PaymentID, bob, nil, 403)

	h.expect("GET", "/reservations/"+res.ID, alice, nil, 200)
	h.expect("GET", "/reservations/"+res.ID, adminTok, nil, 200)
	h.expect("GET", "/users/"+aliceID, adminTok, nil, 200)
	h.expect("GET", "/payments/"+started.PaymentID, providerTok, nil, 200)
	h.expect("GET", "/purchases/"+started.PurchaseID, providerTok, nil, 403)

	h.expect("POST", "/reservations", bob, map[string]any{"userId": aliceID, "eventId": ev, "seatId": "A1"}, 403)
}

func TestInsufficientFunds(t *testing.T) {
	h := newHarness(t)
	ev, _ := h.event(0, 2000)
	_, tok := h.buyer(500)
	providerTok := h.staff(domain.RoleProvider)
	var res domain.Reservation
	h.expect("POST", "/reservations", tok, map[string]any{"eventId": ev, "seatId": "A1"}, 201).json(t, &res)
	var started domain.PurchaseStarted
	h.expect("POST", "/reservations/"+res.ID+"/purchase", tok, map[string]any{}, 201).json(t, &started)

	r := h.expect("POST", "/payments/"+started.PaymentID+"/confirm", providerTok, nil, 402)
	var body errorBody
	r.json(t, &body)
	if body.Error != "insufficient_funds" || body.Payment == nil || body.Payment.Status != domain.PaymentFailed {
		t.Fatalf("body: %+v", body)
	}
	var seats []domain.Seat
	h.expect("GET", "/events/"+ev+"/seats", "", nil, 200).json(t, &seats)
	if seats[0].Status != domain.SeatAvailable {
		t.Fatalf("seat: %+v", seats[0])
	}
}

func TestConcurrentReserve(t *testing.T) {
	h := newHarness(t)
	ev, _ := h.event(0, 2000)
	tokens := make([]string, 100)
	for i := range tokens {
		_, tokens[i] = h.buyer(0)
	}
	var created, conflict atomic.Int32
	var wg sync.WaitGroup
	for _, tok := range tokens {
		wg.Go(func() {
			switch r := h.call("POST", "/reservations", tok, map[string]any{"eventId": ev, "seatId": "A1"}); r.status {
			case 201:
				created.Add(1)
			case 409:
				conflict.Add(1)
			default:
				t.Errorf("status %d: %s", r.status, r.body)
			}
		})
	}
	wg.Wait()
	if created.Load() != 1 || conflict.Load() != 99 {
		t.Fatalf("created=%d conflict=%d", created.Load(), conflict.Load())
	}
}

func TestRefreshAndLogout(t *testing.T) {
	h := newHarness(t)
	login := ids.New("l-")
	h.expect("POST", "/users", "", map[string]any{"name": "n", "login": login, "password": "secret-pass", "balance": 0}, 201)
	h.expect("POST", "/auth/login", "", map[string]any{"login": login, "password": "wrong-pass"}, 401)

	var pair auth.TokenPair
	h.expect("POST", "/auth/login", "", map[string]any{"login": login, "password": "secret-pass"}, 200).json(t, &pair)
	var next auth.TokenPair
	h.expect("POST", "/auth/refresh", "", map[string]any{"refreshToken": pair.RefreshToken}, 200).json(t, &next)
	h.expect("GET", "/users/me", next.AccessToken, nil, 200)
	h.expect("POST", "/auth/logout", "", map[string]any{"refreshToken": next.RefreshToken}, 204)
	if code := h.expect("POST", "/auth/refresh", "", map[string]any{"refreshToken": next.RefreshToken}, 401).errorCode(t); code != "invalid_token" {
		t.Fatalf("code %s", code)
	}
}

func TestRequestValidation(t *testing.T) {
	h := newHarness(t)
	ev, _ := h.event(0, 2000)
	_, tok := h.buyer(0)
	cases := map[string]any{
		"client-chosen price": map[string]any{"eventId": ev, "seatId": "A1", "price": 1},
		"two objects":         `{"eventId":"` + ev + `","seatId":"A1"} {}`,
		"not json":            "seat A1 please",
		"empty body":          nil,
		"missing seat":        map[string]any{"eventId": ev},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if code := h.expect("POST", "/reservations", tok, body, 400).errorCode(t); code != "validation_failed" {
				t.Fatalf("code %s", code)
			}
		})
	}
	h.expect("POST", "/users", "", map[string]any{"name": "n", "login": "x", "password": "p", "balance": 0}, 400)
	h.expect("GET", "/events/missing/seats", "", nil, 404)
	big := `{"name":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	if code := h.expect("POST", "/users", "", big, 413).errorCode(t); code != "body_too_large" {
		t.Fatalf("code %s", code)
	}

	admin := h.staff(domain.RoleAdmin)
	_, org := h.event(0, 1)
	h.expect("POST", "/events", admin, `{"name":"e","organizerId":"`+org+`","seats":[{"number":"X","price":1}],
		"generate":{"prefix":"A","count":9223372036854775807,"price":1}}`, 400)
}

func TestRequestTimeout(t *testing.T) {
	h := newHarnessWithTimeout(t, 200*time.Millisecond)
	ev, _ := h.event(0, 2000)
	_, tok := h.buyer(0)

	tx, err := pgtest.Pool(t).Begin(ctx)
	must(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT 1 FROM seats WHERE event_id = $1 AND number = 'A1' FOR UPDATE`, ev)
	must(t, err)

	start := time.Now()
	if code := h.expect("POST", "/reservations", tok, map[string]any{"eventId": ev, "seatId": "A1"}, 503).errorCode(t); code != "timeout" {
		t.Fatalf("code %s", code)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %s", took)
	}
	must(t, tx.Rollback(ctx))
	var seats []domain.Seat
	h.expect("GET", "/events/"+ev+"/seats", "", nil, 200).json(t, &seats)
	if seats[0].Status != domain.SeatAvailable {
		t.Fatalf("seat: %+v", seats[0])
	}
}

func TestRequestID(t *testing.T) {
	h := newHarness(t)
	r := h.call("GET", "/healthz", "", nil)
	if !strings.HasPrefix(r.header.Get("X-Request-ID"), "req-") {
		t.Fatalf("generated request id: %q", r.header.Get("X-Request-ID"))
	}
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") != "abc-123" {
		t.Fatal("client request id must be propagated")
	}
}

func TestPanicBecomes500(t *testing.T) {
	s := &server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := withRequestID(s.accessLog(s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"internal"`) {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
}

func TestOpenAPIMatchesRoutes(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]struct {
			Security []map[string][]string `json:"security"`
		} `json:"paths"`
	}
	must(t, json.Unmarshal(openAPISpec, &spec))
	documented := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			documented[strings.ToUpper(method)+" "+path] = true
		}
	}
	s := &server{}
	for _, rt := range s.routes() {
		key := rt.method + " " + rt.pattern
		if rt.pattern == "/docs" || rt.pattern == "/openapi.json" {
			continue
		}
		if !documented[key] {
			t.Errorf("%s is not documented", key)
			continue
		}
		delete(documented, key)
		op := spec.Paths[rt.pattern][strings.ToLower(rt.method)]
		if secured := len(op.Security) > 0; secured == rt.access.public {
			t.Errorf("%s: documented security=%v, route public=%v", key, secured, rt.access.public)
		}
	}
	for key := range documented {
		t.Errorf("%s is documented but not routed", key)
	}
}
