package booking

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/ids"
	"awesomeProject/internal/postgres"
	"awesomeProject/internal/testutil/pgtest"
)

type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	svc   *Service
	org   string
	event string
}

func newFixture(t *testing.T, resTTL, payTimeout time.Duration, orgBalance int64, prices ...int64) *fixture {
	t.Helper()
	pool := pgtest.Pool(t)
	svc, err := New(pool, Config{ReservationTTL: resTTL, PaymentTimeout: payTimeout})
	must(t, err)
	org, err := svc.CreateOrganizer(context.Background(), NewOrganizer{Name: "org", Balance: orgBalance})
	must(t, err)
	seats := make([]domain.SeatSpec, len(prices))
	for i, p := range prices {
		seats[i] = domain.SeatSpec{Number: fmt.Sprintf("A%d", i+1), Price: p}
	}
	ev, err := svc.CreateEvent(context.Background(), NewEvent{Name: "concert", OrganizerID: org.ID, Seats: seats})
	must(t, err)
	return &fixture{t: t, pool: pool, svc: svc, org: org.ID, event: ev.ID}
}

func (f *fixture) withService(svc *Service) *fixture {
	c := *f
	c.svc = svc
	return &c
}

func (f *fixture) user(balance int64) string {
	return createUser(f.t, f.pool, domain.RoleUser, balance)
}

func createUser(t *testing.T, pool *pgxpool.Pool, role domain.Role, balance int64) string {
	t.Helper()
	id := ids.New("user-")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, name, login, password_hash, role, balance) VALUES ($1, 'test', $1, '-', $2, $3)`,
		id, role, balance)
	must(t, err)
	return id
}

func (f *fixture) reserve(userID, seat string) *domain.Reservation {
	f.t.Helper()
	r, err := f.svc.Reserve(context.Background(), userID, f.event, seat)
	must(f.t, err)
	return r
}

func (f *fixture) startPurchase(userID, seat string) *domain.PurchaseStarted {
	f.t.Helper()
	r := f.reserve(userID, seat)
	p, err := f.svc.StartPurchase(context.Background(), r.ID, userID)
	must(f.t, err)
	return p
}

func (f *fixture) seatStatus(seat string) domain.SeatStatus {
	f.t.Helper()
	seats, err := f.svc.ListSeats(context.Background(), f.event)
	must(f.t, err)
	for _, s := range seats {
		if s.Number == seat {
			return s.Status
		}
	}
	f.t.Fatalf("seat %s not found", seat)
	return ""
}

func (f *fixture) storedSeatStatus(seat string) domain.SeatStatus {
	f.t.Helper()
	var st domain.SeatStatus
	must(f.t, f.pool.QueryRow(context.Background(),
		`SELECT status FROM seats WHERE event_id = $1 AND number = $2`, f.event, seat).Scan(&st))
	return st
}

func (f *fixture) balances(userID string) (user, org int64) {
	f.t.Helper()
	u, err := f.svc.GetUser(context.Background(), userID)
	must(f.t, err)
	o, err := f.svc.GetOrganizer(context.Background(), f.org)
	must(f.t, err)
	return u.Balance, o.Balance
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	must(f.t, f.pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t testing.TB, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("want error %v, got %v", target, err)
	}
}

func eq[T comparable](t testing.TB, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func parallel(n int, fn func(i int)) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := range n {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			fn(i)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
}

var ctx = context.Background()

func TestReserve(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 0, 2000)
	u := f.user(0)
	eq(t, "initial seat", f.seatStatus("A1"), domain.SeatAvailable)

	r := f.reserve(u, "A1")
	eq(t, "reservation", r.Status, domain.ReservationReserved)
	eq(t, "seat", f.seatStatus("A1"), domain.SeatReserved)
	if !r.ExpiresAt.After(r.CreatedAt) {
		t.Fatal("expiresAt must be after createdAt")
	}
	eq(t, "timestamps are UTC", r.ExpiresAt.Location(), time.UTC)

	_, err := f.svc.Reserve(ctx, f.user(0), f.event, "A1")
	wantErr(t, err, domain.ErrSeatUnavailable)
	_, err = f.svc.Reserve(ctx, u, f.event, "Z99")
	wantErr(t, err, domain.ErrNotFound)
	_, err = f.svc.Reserve(ctx, "nobody", f.event, "A1")
	wantErr(t, err, domain.ErrNotFound)
	_, err = f.svc.Reserve(ctx, u, f.event, "")
	wantErr(t, err, domain.ErrValidation)
}

func TestStaffCannotReserve(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 0, 2000)
	_, err := f.svc.Reserve(ctx, createUser(t, f.pool, domain.RoleAdmin, 0), f.event, "A1")
	wantErr(t, err, domain.ErrForbidden)
}

func TestConcurrentReserveLastSeat(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 0, 2000)
	users := make([]string, 100)
	for i := range users {
		users[i] = f.user(0)
	}
	var ok, conflict atomic.Int32
	parallel(len(users), func(i int) {
		_, err := f.svc.Reserve(ctx, users[i], f.event, "A1")
		switch {
		case err == nil:
			ok.Add(1)
		case errors.Is(err, domain.ErrSeatUnavailable):
			conflict.Add(1)
		default:
			t.Error(err)
		}
	})
	eq(t, "success", ok.Load(), 1)
	eq(t, "conflicts", conflict.Load(), 99)
	eq(t, "live reservations", f.count(
		`SELECT count(*) FROM reservations WHERE event_id = $1 AND status = 'reserved'`, f.event), 1)
}

func TestTwoInstancesSameSeat(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 0, 2000)
	poolB, err := postgres.Connect(ctx, pgtest.URL(), 10)
	must(t, err)
	defer poolB.Close()
	svcB, err := New(poolB, f.svc.cfg)
	must(t, err)
	users := make([]string, 60)
	for i := range users {
		users[i] = f.user(0)
	}
	var ok atomic.Int32
	parallel(len(users), func(i int) {
		svc := f.svc
		if i%2 == 1 {
			svc = svcB
		}
		if _, err := svc.Reserve(ctx, users[i], f.event, "A1"); err == nil {
			ok.Add(1)
		}
	})
	eq(t, "success across instances", ok.Load(), 1)
}

func TestCancel(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 0, 2000)
	u := f.user(0)
	r := f.reserve(u, "A1")

	_, err := f.svc.CancelReservation(ctx, r.ID, f.user(0))
	wantErr(t, err, domain.ErrForbidden)

	c, err := f.svc.CancelReservation(ctx, r.ID, u)
	must(t, err)
	eq(t, "status", c.Status, domain.ReservationCanceled)
	if c.FinishedAt == nil {
		t.Fatal("finishedAt must be set")
	}
	eq(t, "seat", f.seatStatus("A1"), domain.SeatAvailable)
	_, err = f.svc.CancelReservation(ctx, r.ID, u)
	wantErr(t, err, domain.ErrInvalidTransition)
	f.reserve(f.user(0), "A1")
}

func TestCannotCancelExpiredOrPurchased(t *testing.T) {
	f := newFixture(t, 30*time.Millisecond, time.Minute, 0, 2000)
	u := f.user(5000)
	r := f.reserve(u, "A1")
	time.Sleep(60 * time.Millisecond)
	_, err := f.svc.CancelReservation(ctx, r.ID, u)
	wantErr(t, err, domain.ErrReservationExpired)
	eq(t, "seat released", f.storedSeatStatus("A1"), domain.SeatAvailable)

	g := newFixture(t, time.Minute, time.Minute, 0, 2000)
	u2 := g.user(5000)
	p := g.startPurchase(u2, "A1")
	_, err = g.svc.ConfirmPayment(ctx, p.PaymentID)
	must(t, err)
	pay, _ := g.svc.GetPayment(ctx, p.PaymentID)
	_, err = g.svc.CancelReservation(ctx, pay.ReservationID, u2)
	wantErr(t, err, domain.ErrInvalidTransition)
	eq(t, "seat stays sold", g.storedSeatStatus("A1"), domain.SeatPurchased)
}

func TestExpiration(t *testing.T) {
	f := newFixture(t, 50*time.Millisecond, time.Minute, 0, 2000)
	r := f.reserve(f.user(0), "A1")
	time.Sleep(80 * time.Millisecond)

	eq(t, "effective seat", f.seatStatus("A1"), domain.SeatAvailable)
	eq(t, "stored seat", f.storedSeatStatus("A1"), domain.SeatReserved)
	got, err := f.svc.GetReservation(ctx, r.ID)
	must(t, err)
	eq(t, "effective reservation", got.Status, domain.ReservationExpired)

	n, err := f.svc.ExpireDueReservations(ctx)
	must(t, err)
	if n < 1 {
		t.Fatalf("sweeper expired %d reservations", n)
	}
	eq(t, "stored seat after sweep", f.storedSeatStatus("A1"), domain.SeatAvailable)
	again, err := f.svc.ExpireReservation(ctx, r.ID)
	must(t, err)
	eq(t, "second expire is a no-op", again, false)
}

func TestSweeperDoesNotWaitForeverOnALockedSeat(t *testing.T) {
	f := newFixture(t, 30*time.Millisecond, time.Minute, 0, 2000, 2000)
	f.svc.cfg.SweepItemTimeout = 200 * time.Millisecond
	stuck := f.reserve(f.user(0), "A1")
	free := f.reserve(f.user(0), "A2")
	time.Sleep(60 * time.Millisecond)

	tx, err := f.pool.Begin(ctx)
	must(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT 1 FROM seats WHERE event_id = $1 AND number = 'A1' FOR UPDATE`, f.event)
	must(t, err)

	start := time.Now()
	_, err = f.svc.ExpireDueReservations(ctx)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the pass took %s", took)
	}
	if err == nil || !strings.Contains(err.Error(), stuck.ID) {
		t.Fatalf("want an error about %s, got %v", stuck.ID, err)
	}
	eq(t, "free seat released", f.storedSeatStatus("A2"), domain.SeatAvailable)
	var status domain.ReservationStatus
	must(t, f.pool.QueryRow(ctx, `SELECT status FROM reservations WHERE id = $1`, free.ID).Scan(&status))
	eq(t, "free reservation", status, domain.ReservationExpired)

	must(t, tx.Rollback(ctx))
	_, err = f.svc.ExpireDueReservations(ctx)
	must(t, err)
	eq(t, "locked seat released on the next pass", f.storedSeatStatus("A1"), domain.SeatAvailable)
}

func TestReserveOverExpiredReservationWithoutSweeper(t *testing.T) {
	f := newFixture(t, 30*time.Millisecond, time.Minute, 0, 2000)
	old := f.reserve(f.user(0), "A1")
	time.Sleep(60 * time.Millisecond)
	fresh := f.reserve(f.user(0), "A1")
	if fresh.ID == old.ID {
		t.Fatal("expected a new reservation")
	}
	got, _ := f.svc.GetReservation(ctx, old.ID)
	eq(t, "old reservation", got.Status, domain.ReservationExpired)
}

func TestReserveOverTimedOutPaymentWithoutSweeper(t *testing.T) {
	f := newFixture(t, time.Minute, 40*time.Millisecond, 50000, 2000)
	u1 := f.user(10000)
	p := f.startPurchase(u1, "A1")
	time.Sleep(70 * time.Millisecond)
	eq(t, "effective seat", f.seatStatus("A1"), domain.SeatAvailable)

	f.reserve(f.user(0), "A1")
	pay, err := f.svc.GetPayment(ctx, p.PaymentID)
	must(t, err)
	eq(t, "old payment", pay.Status, domain.PaymentTimedOut)
	eq(t, "old purchase", pay.PurchaseStatus, domain.PurchaseTimedOut)
	_, err = f.svc.ConfirmPayment(ctx, p.PaymentID)
	wantErr(t, err, domain.ErrPaymentTimedOut)
	ub, ob := f.balances(u1)
	eq(t, "user1 balance", ub, 10000)
	eq(t, "organizer balance", ob, 50000)
}

func TestStartPurchase(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 0, 2000)
	u := f.user(10000)
	r := f.reserve(u, "A1")
	_, err := f.svc.StartPurchase(ctx, r.ID, f.user(0))
	wantErr(t, err, domain.ErrForbidden)

	p, err := f.svc.StartPurchase(ctx, r.ID, u)
	must(t, err)
	eq(t, "status", p.Status, domain.PurchasePaymentPending)
	eq(t, "amount is the server-side price", p.Amount, 2000)
	eq(t, "seat", f.seatStatus("A1"), domain.SeatPaymentPending)
	got, _ := f.svc.GetReservation(ctx, r.ID)
	eq(t, "reservation", got.Status, domain.ReservationPaymentPending)

	_, err = f.svc.StartPurchase(ctx, r.ID, u)
	wantErr(t, err, domain.ErrInvalidTransition)
	_, err = f.svc.CancelReservation(ctx, r.ID, u)
	wantErr(t, err, domain.ErrInvalidTransition)
	_, err = f.svc.Reserve(ctx, f.user(0), f.event, "A1")
	wantErr(t, err, domain.ErrSeatUnavailable)
}

func TestPurchaseAfterReservationTTL(t *testing.T) {
	f := newFixture(t, 30*time.Millisecond, time.Minute, 0, 2000)
	u := f.user(10000)
	r := f.reserve(u, "A1")
	time.Sleep(60 * time.Millisecond)
	_, err := f.svc.StartPurchase(ctx, r.ID, u)
	wantErr(t, err, domain.ErrReservationExpired)
	eq(t, "seat", f.storedSeatStatus("A1"), domain.SeatAvailable)
}

func TestSuccessfulPayment(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 50000, 2000)
	u := f.user(10000)
	p := f.startPurchase(u, "A1")
	ub, ob := f.balances(u)
	eq(t, "user before", ub, 10000)
	eq(t, "organizer before", ob, 50000)

	pay, err := f.svc.ConfirmPayment(ctx, p.PaymentID)
	must(t, err)
	eq(t, "payment", pay.Status, domain.PaymentSucceeded)
	eq(t, "purchase", pay.PurchaseStatus, domain.PurchasePurchased)
	if pay.CompletedAt == nil {
		t.Fatal("completedAt must be set")
	}
	ub, ob = f.balances(u)
	eq(t, "user after", ub, 8000)
	eq(t, "organizer after", ob, 52000)
	eq(t, "seat", f.seatStatus("A1"), domain.SeatPurchased)
	pu, err := f.svc.GetPurchase(ctx, p.PurchaseID)
	must(t, err)
	eq(t, "purchase", pu.Status, domain.PurchasePurchased)
	_, err = f.svc.Reserve(ctx, u, f.event, "A1")
	wantErr(t, err, domain.ErrSeatUnavailable)
}

func TestReservationHistory(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 0, 2000)
	u := f.user(10000)
	p := f.startPurchase(u, "A1")
	_, err := f.svc.ConfirmPayment(ctx, p.PaymentID)
	must(t, err)
	pay, _ := f.svc.GetPayment(ctx, p.PaymentID)
	history, err := f.svc.ReservationHistory(ctx, pay.ReservationID)
	must(t, err)
	var got []string
	for _, h := range history {
		got = append(got, h.Entity+":"+h.To)
	}
	want := []string{
		"reservation:reserved",
		"purchase:payment_pending", "payment:pending", "reservation:payment_pending",
		"payment:succeeded", "purchase:purchased", "reservation:purchased",
	}
	eq(t, "timeline", fmt.Sprint(got), fmt.Sprint(want))
}

func TestInsufficientFunds(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 50000, 2000)
	u := f.user(500)
	p := f.startPurchase(u, "A1")
	pay, err := f.svc.ConfirmPayment(ctx, p.PaymentID)
	wantErr(t, err, domain.ErrInsufficientFunds)
	eq(t, "payment", pay.Status, domain.PaymentFailed)
	eq(t, "reason", pay.FailureReason, domain.FailureInsufficientFunds)
	eq(t, "purchase", pay.PurchaseStatus, domain.PurchaseFailed)
	ub, ob := f.balances(u)
	eq(t, "user", ub, 500)
	eq(t, "organizer", ob, 50000)
	eq(t, "seat", f.seatStatus("A1"), domain.SeatAvailable)
	got, _ := f.svc.GetPayment(ctx, p.PaymentID)
	eq(t, "failure is persisted", got.Status, domain.PaymentFailed)
	_, err = f.svc.ConfirmPayment(ctx, p.PaymentID)
	wantErr(t, err, domain.ErrPaymentNotPending)
}

func TestFailPayment(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 50000, 2000)
	u := f.user(10000)
	p := f.startPurchase(u, "A1")
	pay, err := f.svc.FailPayment(ctx, p.PaymentID)
	must(t, err)
	eq(t, "payment", pay.Status, domain.PaymentFailed)
	eq(t, "reason", pay.FailureReason, domain.FailureDeclined)
	eq(t, "purchase", pay.PurchaseStatus, domain.PurchaseFailed)
	_, err = f.svc.FailPayment(ctx, p.PaymentID)
	must(t, err)
	ub, ob := f.balances(u)
	eq(t, "user", ub, 10000)
	eq(t, "organizer", ob, 50000)
	eq(t, "seat", f.seatStatus("A1"), domain.SeatAvailable)
	_, err = f.svc.ConfirmPayment(ctx, p.PaymentID)
	wantErr(t, err, domain.ErrPaymentNotPending)
}

func TestPaymentTimeout(t *testing.T) {
	f := newFixture(t, time.Minute, 50*time.Millisecond, 50000, 2000)
	u := f.user(10000)
	p := f.startPurchase(u, "A1")
	time.Sleep(80 * time.Millisecond)
	got, _ := f.svc.GetPayment(ctx, p.PaymentID)
	eq(t, "effective payment", got.Status, domain.PaymentTimedOut)

	n, err := f.svc.TimeoutDuePayments(ctx)
	must(t, err)
	if n < 1 {
		t.Fatalf("sweeper timed out %d payments", n)
	}
	got, _ = f.svc.GetPayment(ctx, p.PaymentID)
	eq(t, "payment", got.Status, domain.PaymentTimedOut)
	eq(t, "purchase", got.PurchaseStatus, domain.PurchaseTimedOut)
	eq(t, "seat", f.storedSeatStatus("A1"), domain.SeatAvailable)
	ub, ob := f.balances(u)
	eq(t, "user", ub, 10000)
	eq(t, "organizer", ob, 50000)
}

func TestDelayedConfirm(t *testing.T) {
	f := newFixture(t, 150*time.Millisecond, time.Minute, 0, 2000)
	u := f.user(10000)
	p := f.startPurchase(u, "A1")
	time.Sleep(250 * time.Millisecond)
	_, err := f.svc.ExpireDueReservations(ctx)
	must(t, err)
	eq(t, "seat still held by the purchase", f.seatStatus("A1"), domain.SeatPaymentPending)

	pay, err := f.svc.ConfirmPayment(ctx, p.PaymentID)
	must(t, err)
	eq(t, "payment", pay.Status, domain.PaymentSucceeded)
	eq(t, "seat", f.seatStatus("A1"), domain.SeatPurchased)
}

func TestLateConfirm(t *testing.T) {
	f := newFixture(t, time.Minute, 50*time.Millisecond, 50000, 2000)
	u1, u2 := f.user(10000), f.user(10000)
	p := f.startPurchase(u1, "A1")
	time.Sleep(80 * time.Millisecond)
	_, err := f.svc.TimeoutDuePayments(ctx)
	must(t, err)
	f.reserve(u2, "A1")

	pay, err := f.svc.ConfirmPayment(ctx, p.PaymentID)
	wantErr(t, err, domain.ErrPaymentTimedOut)
	eq(t, "payment", pay.Status, domain.PaymentTimedOut)
	ub, ob := f.balances(u1)
	eq(t, "user1", ub, 10000)
	eq(t, "organizer", ob, 50000)
	eq(t, "seat stays with user 2", f.seatStatus("A1"), domain.SeatReserved)
}

func TestLateConfirmWithoutSweeper(t *testing.T) {
	f := newFixture(t, time.Minute, 40*time.Millisecond, 50000, 2000)
	u := f.user(10000)
	p := f.startPurchase(u, "A1")
	time.Sleep(70 * time.Millisecond)
	_, err := f.svc.ConfirmPayment(ctx, p.PaymentID)
	wantErr(t, err, domain.ErrPaymentTimedOut)
	ub, ob := f.balances(u)
	eq(t, "user", ub, 10000)
	eq(t, "organizer", ob, 50000)
	eq(t, "seat", f.storedSeatStatus("A1"), domain.SeatAvailable)
}

func TestDuplicateConfirm(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 50000, 2000, 2000)
	u := f.user(10000)
	p := f.startPurchase(u, "A1")
	for range 3 {
		pay, err := f.svc.ConfirmPayment(ctx, p.PaymentID)
		must(t, err)
		eq(t, "payment", pay.Status, domain.PaymentSucceeded)
	}
	ub, ob := f.balances(u)
	eq(t, "user", ub, 8000)
	eq(t, "organizer", ob, 52000)

	u2 := f.user(10000)
	p2 := f.startPurchase(u2, "A2")
	parallel(20, func(int) {
		if _, err := f.svc.ConfirmPayment(ctx, p2.PaymentID); err != nil {
			t.Error(err)
		}
	})
	ub, ob = f.balances(u2)
	eq(t, "user2 charged once", ub, 8000)
	eq(t, "organizer credited once", ob, 54000)
	eq(t, "one success transition", f.count(
		`SELECT count(*) FROM transitions WHERE entity_id = $1 AND to_status = 'succeeded'`, p2.PaymentID), 1)
}

func TestDoubleSpending(t *testing.T) {
	for range 20 {
		f := newFixture(t, time.Minute, time.Minute, 0, 2000, 2000)
		u := f.user(2000)
		a, b := f.startPurchase(u, "A1"), f.startPurchase(u, "A2")
		var ok, insufficient atomic.Int32
		parallel(2, func(i int) {
			_, err := f.svc.ConfirmPayment(ctx, []string{a.PaymentID, b.PaymentID}[i])
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, domain.ErrInsufficientFunds):
				insufficient.Add(1)
			default:
				t.Error(err)
			}
		})
		eq(t, "succeeded", ok.Load(), 1)
		eq(t, "insufficient_funds", insufficient.Load(), 1)
		ub, ob := f.balances(u)
		eq(t, "user", ub, 0)
		eq(t, "organizer", ob, 2000)
	}
}

func TestBalanceHotspot(t *testing.T) {
	prices := make([]int64, 30)
	for i := range prices {
		prices[i] = 100
	}
	f := newFixture(t, time.Minute, time.Minute, 0, prices...)
	u := f.user(1000)
	payments := make([]string, len(prices))
	for i := range payments {
		payments[i] = f.startPurchase(u, fmt.Sprintf("A%d", i+1)).PaymentID
	}
	var ok atomic.Int32
	parallel(len(payments), func(i int) {
		_, err := f.svc.ConfirmPayment(ctx, payments[i])
		if err == nil {
			ok.Add(1)
		} else if !errors.Is(err, domain.ErrInsufficientFunds) {
			t.Error(err)
		}
	})
	eq(t, "succeeded", ok.Load(), 10)
	ub, ob := f.balances(u)
	eq(t, "user", ub, 0)
	eq(t, "organizer", ob, 1000)
}

func TestCancelVsPurchase(t *testing.T) {
	for range 40 {
		f := newFixture(t, time.Minute, time.Minute, 0, 2000)
		u := f.user(10000)
		r := f.reserve(u, "A1")
		var cancelErr, purchaseErr error
		parallel(2, func(i int) {
			if i == 0 {
				_, cancelErr = f.svc.CancelReservation(ctx, r.ID, u)
			} else {
				_, purchaseErr = f.svc.StartPurchase(ctx, r.ID, u)
			}
		})
		got, _ := f.svc.GetReservation(ctx, r.ID)
		switch got.Status {
		case domain.ReservationCanceled:
			must(t, cancelErr)
			wantErr(t, purchaseErr, domain.ErrInvalidTransition)
			eq(t, "seat", f.storedSeatStatus("A1"), domain.SeatAvailable)
			eq(t, "purchases", f.count(`SELECT count(*) FROM purchases WHERE reservation_id = $1`, r.ID), 0)
		case domain.ReservationPaymentPending:
			must(t, purchaseErr)
			wantErr(t, cancelErr, domain.ErrInvalidTransition)
			eq(t, "seat", f.storedSeatStatus("A1"), domain.SeatPaymentPending)
		default:
			t.Fatalf("unexpected status %s", got.Status)
		}
	}
}

func TestExpireVsPurchase(t *testing.T) {
	var expired, pending int
	for i := range 60 {
		f := newFixture(t, 25*time.Millisecond, time.Minute, 0, 2000)
		u := f.user(10000)
		r := f.reserve(u, "A1")
		time.Sleep(time.Until(r.ExpiresAt) - time.Duration(i%6)*time.Millisecond)
		var purchaseErr error
		parallel(2, func(k int) {
			if k == 0 {
				_, _ = f.svc.ExpireReservation(ctx, r.ID)
			} else {
				_, purchaseErr = f.svc.StartPurchase(ctx, r.ID, u)
			}
		})
		got, _ := f.svc.GetReservation(ctx, r.ID)
		switch got.Status {
		case domain.ReservationExpired:
			expired++
			if purchaseErr == nil {
				t.Fatal("purchase succeeded but the reservation is expired")
			}
			eq(t, "seat", f.storedSeatStatus("A1"), domain.SeatAvailable)
		case domain.ReservationPaymentPending:
			pending++
			must(t, purchaseErr)
			eq(t, "seat", f.storedSeatStatus("A1"), domain.SeatPaymentPending)
			eq(t, "purchases", f.count(`SELECT count(*) FROM purchases WHERE reservation_id = $1`, r.ID), 1)
		default:
			t.Fatalf("unexpected status %s", got.Status)
		}
	}
	t.Logf("expire vs purchase: expired=%d payment_pending=%d", expired, pending)
}

func TestTimeoutVsConfirm(t *testing.T) {
	var timedOut, purchased int
	for i := range 60 {
		f := newFixture(t, time.Minute, 40*time.Millisecond, 50000, 2000)
		u := f.user(10000)
		p := f.startPurchase(u, "A1")
		time.Sleep(time.Until(p.PaymentExpiresAt) - time.Duration(i%6)*time.Millisecond)
		parallel(2, func(k int) {
			if k == 0 {
				_, _ = f.svc.TimeoutPayment(ctx, p.PaymentID)
			} else {
				_, _ = f.svc.ConfirmPayment(ctx, p.PaymentID)
			}
		})
		pay, _ := f.svc.GetPayment(ctx, p.PaymentID)
		ub, ob := f.balances(u)
		switch pay.Status {
		case domain.PaymentSucceeded:
			purchased++
			eq(t, "purchase", pay.PurchaseStatus, domain.PurchasePurchased)
			eq(t, "user", ub, 8000)
			eq(t, "organizer", ob, 52000)
			eq(t, "seat", f.storedSeatStatus("A1"), domain.SeatPurchased)
		case domain.PaymentTimedOut:
			timedOut++
			eq(t, "purchase", pay.PurchaseStatus, domain.PurchaseTimedOut)
			eq(t, "user", ub, 10000)
			eq(t, "organizer", ob, 50000)
			eq(t, "seat", f.storedSeatStatus("A1"), domain.SeatAvailable)
		default:
			t.Fatalf("unexpected payment status %s", pay.Status)
		}
	}
	t.Logf("timeout vs confirm: purchased=%d timed_out=%d", purchased, timedOut)
}

func TestRestart(t *testing.T) {
	f := newFixture(t, 30*time.Millisecond, 40*time.Millisecond, 50000, 2000, 2000)
	u := f.user(10000)
	f.reserve(u, "A1")
	p := f.startPurchase(u, "A2")
	time.Sleep(80 * time.Millisecond)

	restarted, err := New(f.pool, Config{ReservationTTL: time.Minute, PaymentTimeout: time.Minute})
	must(t, err)
	g := f.withService(restarted)
	eq(t, "A1 (reservation expired)", g.seatStatus("A1"), domain.SeatAvailable)
	eq(t, "A2 (payment expired)", g.seatStatus("A2"), domain.SeatAvailable)
	_, err = restarted.ConfirmPayment(ctx, p.PaymentID)
	wantErr(t, err, domain.ErrPaymentTimedOut)
	g.reserve(g.user(0), "A1")
	g.reserve(g.user(0), "A2")
}

func TestCancelledContextRollsBack(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 50000, 2000)
	u := f.user(10000)
	p := f.startPurchase(u, "A1")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := f.svc.ConfirmPayment(cancelled, p.PaymentID)
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	ub, ob := f.balances(u)
	eq(t, "user", ub, 10000)
	eq(t, "organizer", ob, 50000)
	got, _ := f.svc.GetPayment(ctx, p.PaymentID)
	eq(t, "payment", got.Status, domain.PaymentPending)
}

func TestStoreRefusesTransitionsFromFinalStates(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 0, 2000)
	u := f.user(10000)
	p := f.startPurchase(u, "A1")
	_, err := f.svc.ConfirmPayment(ctx, p.PaymentID)
	must(t, err)
	pay, _ := f.svc.GetPayment(ctx, p.PaymentID)

	st := store{q: f.pool}
	err = st.moveReservation(ctx, pay.ReservationID, domain.ReservationPurchased, domain.ReservationCanceled, "test")
	wantErr(t, err, domain.ErrInvalidTransition)
	err = st.moveReservation(ctx, pay.ReservationID, domain.ReservationReserved, domain.ReservationCanceled, "test")
	wantErr(t, err, domain.ErrInvalidTransition)
	got, _ := f.svc.GetReservation(ctx, pay.ReservationID)
	eq(t, "reservation unchanged", got.Status, domain.ReservationPurchased)
}

func TestHistoryOfUnknownReservationIsEmpty(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 0, 2000)
	r := f.reserve(f.user(0), "A1")
	h, err := f.svc.ReservationHistory(ctx, r.ID)
	must(t, err)
	eq(t, "entries before purchase", len(h), 1)
	h, err = f.svc.ReservationHistory(ctx, "r-unknown")
	must(t, err)
	eq(t, "unknown reservation", len(h), 0)
}

func seat(number string, price int64) domain.SeatSpec {
	return domain.SeatSpec{Number: number, Price: price}
}

func TestCreateEventValidation(t *testing.T) {
	f := newFixture(t, time.Minute, time.Minute, 0, 2000)
	cases := map[string]NewEvent{
		"no seats":       {Name: "e", OrganizerID: f.org},
		"duplicate seat": {Name: "e", OrganizerID: f.org, Seats: []domain.SeatSpec{seat("A1", 1), seat("A1", 1)}},
		"zero price":     {Name: "e", OrganizerID: f.org, Seats: []domain.SeatSpec{seat("A1", 0)}},
		"bad number":     {Name: "e", OrganizerID: f.org, Seats: []domain.SeatSpec{seat("A 1", 1)}},
		"no name":        {OrganizerID: f.org, Seats: []domain.SeatSpec{seat("A1", 1)}},
		"bad id":         {ID: "white space", Name: "e", OrganizerID: f.org, Seats: []domain.SeatSpec{seat("A1", 1)}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.CreateEvent(ctx, in)
			wantErr(t, err, domain.ErrValidation)
		})
	}
	_, err := f.svc.CreateEvent(ctx, NewEvent{Name: strings.Repeat("я", 200), OrganizerID: f.org, Seats: []domain.SeatSpec{seat("A1", 1)}})
	must(t, err)
	_, err = f.svc.CreateEvent(ctx, NewEvent{Name: "e", OrganizerID: "missing", Seats: []domain.SeatSpec{seat("A1", 1)}})
	wantErr(t, err, domain.ErrNotFound)
	_, err = f.svc.CreateEvent(ctx, NewEvent{ID: f.event, Name: "e", OrganizerID: f.org, Seats: []domain.SeatSpec{seat("A1", 1)}})
	wantErr(t, err, domain.ErrAlreadyExists)
}
