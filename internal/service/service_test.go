package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/tickets?sslmode=disable"
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err == nil {
		cfg.MaxConns = 40
		testPool, err = pgxpool.NewWithConfig(ctx, cfg)
	}
	if err == nil {
		err = testPool.Ping(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: PostgreSQL is not available (%v); set TEST_DATABASE_URL\n", err)
		os.Exit(0)
	}
	if err := Migrate(ctx, testPool); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	code := m.Run()
	testPool.Close()
	os.Exit(code)
}

var ctx = context.Background()

func newSvc(resTTL, payTimeout time.Duration) *Service {
	return New(testPool, Config{ReservationTTL: resTTL, PaymentTimeout: payTimeout})
}

// fixture: one organizer, one event with the given seat prices (A1, A2, ...).
type fixture struct {
	svc   *Service
	org   *Organizer
	event *Event
}

func newFixture(t *testing.T, svc *Service, orgBalance int64, prices ...int64) *fixture {
	t.Helper()
	org, err := svc.CreateOrganizer(ctx, "", "org", orgBalance)
	must(t, err)
	var seats []SeatSpec
	for i, p := range prices {
		seats = append(seats, SeatSpec{Number: fmt.Sprintf("A%d", i+1), Price: p})
	}
	ev, err := svc.CreateEvent(ctx, "", "concert", org.ID, seats)
	must(t, err)
	return &fixture{svc: svc, org: org, event: ev}
}

func (f *fixture) user(t *testing.T, balance int64) *User {
	t.Helper()
	u, err := f.svc.CreateUser(ctx, "", "u", balance)
	must(t, err)
	return u
}

func (f *fixture) reserve(t *testing.T, u *User, seat string) *Reservation {
	t.Helper()
	r, err := f.svc.Reserve(ctx, u.ID, f.event.ID, seat)
	must(t, err)
	return r
}

func (f *fixture) startPurchase(t *testing.T, u *User, seat string) *PurchaseStarted {
	t.Helper()
	r := f.reserve(t, u, seat)
	p, err := f.svc.StartPurchase(ctx, r.ID, u.ID)
	must(t, err)
	return p
}

func (f *fixture) seatStatus(t *testing.T, seat string) string {
	t.Helper()
	seats, err := f.svc.ListSeats(ctx, f.event.ID)
	must(t, err)
	for _, s := range seats {
		if s.Seat == seat {
			return s.Status
		}
	}
	t.Fatalf("seat %s not found", seat)
	return ""
}

// rawSeatStatus reads the stored status, bypassing the "effective" view.
func (f *fixture) rawSeatStatus(t *testing.T, seat string) string {
	t.Helper()
	var st string
	must(t, testPool.QueryRow(ctx, `SELECT status FROM seats WHERE event_id=$1 AND number=$2`, f.event.ID, seat).Scan(&st))
	return st
}

func (f *fixture) balances(t *testing.T, u *User) (user, org int64) {
	t.Helper()
	uu, err := f.svc.GetUser(ctx, u.ID)
	must(t, err)
	oo, err := f.svc.GetOrganizer(ctx, f.org.ID)
	must(t, err)
	return uu.Balance, oo.Balance
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("want error %v, got %v", target, err)
	}
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// parallel runs fn(i) for i in [0,n) starting all goroutines at the same moment.
func parallel(n int, fn func(i int)) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
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

func TestReserve(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, time.Minute), 0, 2000)
	u := f.user(t, 0)
	eq(t, "initial", f.seatStatus(t, "A1"), SeatAvailable)
	r := f.reserve(t, u, "A1")
	eq(t, "status", r.Status, ResReserved)
	eq(t, "seat", f.seatStatus(t, "A1"), SeatReserved)
	if !r.ExpiresAt.After(r.CreatedAt) {
		t.Fatal("expiresAt must be after createdAt")
	}
	_, err := f.svc.Reserve(ctx, f.user(t, 0).ID, f.event.ID, "A1")
	wantErr(t, err, ErrSeatUnavailable)
	_, err = f.svc.Reserve(ctx, u.ID, f.event.ID, "ZZ")
	wantErr(t, err, ErrNotFound)
	_, err = f.svc.Reserve(ctx, "nobody", f.event.ID, "A1")
	wantErr(t, err, ErrNotFound)
}

// Experiment 1: the last seat, 100 concurrent reserve requests.
func TestConcurrentReserveLastSeat(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, time.Minute), 0, 2000)
	users := make([]*User, 100)
	for i := range users {
		users[i] = f.user(t, 0)
	}
	var ok, conflict, other atomic.Int32
	parallel(100, func(i int) {
		_, err := f.svc.Reserve(ctx, users[i].ID, f.event.ID, "A1")
		switch {
		case err == nil:
			ok.Add(1)
		case errors.Is(err, ErrSeatUnavailable):
			conflict.Add(1)
		default:
			other.Add(1)
			t.Error(err)
		}
	})
	eq(t, "success", ok.Load(), 1)
	eq(t, "conflicts", conflict.Load(), 99)
	var live int
	must(t, testPool.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE event_id=$1 AND status='reserved'`, f.event.ID).Scan(&live))
	eq(t, "live reservations in db", live, 1)
}

// Two application instances (two services, two pools) on one database.
func TestTwoInstancesSameSeat(t *testing.T) {
	svcA := newSvc(time.Minute, time.Minute)
	poolB, err := pgxpool.New(ctx, testPool.Config().ConnString())
	must(t, err)
	defer poolB.Close()
	svcB := New(poolB, Config{ReservationTTL: time.Minute, PaymentTimeout: time.Minute})
	f := newFixture(t, svcA, 0, 2000)
	users := make([]*User, 60)
	for i := range users {
		users[i] = f.user(t, 0)
	}
	var ok atomic.Int32
	parallel(60, func(i int) {
		s := svcA
		if i%2 == 1 {
			s = svcB
		}
		if _, err := s.Reserve(ctx, users[i].ID, f.event.ID, "A1"); err == nil {
			ok.Add(1)
		}
	})
	eq(t, "success", ok.Load(), 1)
}

func TestCancel(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, time.Minute), 0, 2000)
	u := f.user(t, 0)
	r := f.reserve(t, u, "A1")
	c, err := f.svc.Cancel(ctx, r.ID)
	must(t, err)
	eq(t, "status", c.Status, ResCanceled)
	eq(t, "seat", f.seatStatus(t, "A1"), SeatAvailable)
	_, err = f.svc.Cancel(ctx, r.ID)
	wantErr(t, err, ErrInvalidState)
	// someone else can take the seat now
	f.reserve(t, f.user(t, 0), "A1")
}

func TestCannotCancelExpiredOrPurchased(t *testing.T) {
	f := newFixture(t, newSvc(30*time.Millisecond, time.Minute), 0, 2000, 2000)
	u := f.user(t, 5000)
	r := f.reserve(t, u, "A1")
	time.Sleep(60 * time.Millisecond)
	_, err := f.svc.Cancel(ctx, r.ID)
	wantErr(t, err, ErrReservationExpired)
	eq(t, "seat", f.rawSeatStatus(t, "A1"), SeatAvailable)

	svc := newSvc(time.Minute, time.Minute)
	f2 := newFixture(t, svc, 0, 2000)
	u2 := f2.user(t, 5000)
	r2 := f2.reserve(t, u2, "A1")
	p, err := svc.StartPurchase(ctx, r2.ID, u2.ID)
	must(t, err)
	_, err = svc.Confirm(ctx, p.PaymentID)
	must(t, err)
	_, err = svc.Cancel(ctx, r2.ID)
	wantErr(t, err, ErrInvalidState)
	eq(t, "seat", f2.rawSeatStatus(t, "A1"), SeatPurchased)
}

func TestExpiration(t *testing.T) {
	f := newFixture(t, newSvc(50*time.Millisecond, time.Minute), 0, 2000)
	r := f.reserve(t, f.user(t, 0), "A1")
	time.Sleep(80 * time.Millisecond)
	// Even before the worker runs, the seat is effectively free.
	eq(t, "effective seat", f.seatStatus(t, "A1"), SeatAvailable)
	eq(t, "stored seat (worker hasn't run)", f.rawSeatStatus(t, "A1"), SeatReserved)
	got, err := f.svc.GetReservation(ctx, r.ID)
	must(t, err)
	eq(t, "effective reservation", got.Status, ResExpired)

	n, err := f.svc.ExpireDueReservations(ctx)
	must(t, err)
	if n < 1 {
		t.Fatalf("worker expired %d reservations", n)
	}
	eq(t, "stored seat after worker", f.rawSeatStatus(t, "A1"), SeatAvailable)
	again, err := f.svc.ExpireReservation(ctx, r.ID)
	must(t, err)
	eq(t, "second expire is a no-op", again, false)
}

// Correctness must not depend on the worker: a stale reservation does not block the seat.
func TestReserveOverStaleReservationWithoutWorker(t *testing.T) {
	f := newFixture(t, newSvc(30*time.Millisecond, time.Minute), 0, 2000)
	old := f.reserve(t, f.user(t, 0), "A1")
	time.Sleep(60 * time.Millisecond)
	fresh := f.reserve(t, f.user(t, 0), "A1")
	if fresh.ID == old.ID {
		t.Fatal("expected a new reservation")
	}
	got, _ := f.svc.GetReservation(ctx, old.ID)
	eq(t, "old", got.Status, ResExpired)
}

func TestStartPurchase(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, time.Minute), 0, 2000)
	u := f.user(t, 10000)
	r := f.reserve(t, u, "A1")
	_, err := f.svc.StartPurchase(ctx, r.ID, "someone-else")
	wantErr(t, err, ErrForbidden)
	p, err := f.svc.StartPurchase(ctx, r.ID, u.ID)
	must(t, err)
	eq(t, "status", p.Status, PurchasePending)
	eq(t, "amount (server-side price)", p.Amount, 2000)
	eq(t, "seat", f.seatStatus(t, "A1"), SeatPaymentPending)
	got, _ := f.svc.GetReservation(ctx, r.ID)
	eq(t, "reservation", got.Status, ResPaymentPending)
	_, err = f.svc.StartPurchase(ctx, r.ID, u.ID)
	wantErr(t, err, ErrInvalidState)
	_, err = f.svc.Cancel(ctx, r.ID)
	wantErr(t, err, ErrInvalidState)
	_, err = f.svc.Reserve(ctx, f.user(t, 0).ID, f.event.ID, "A1")
	wantErr(t, err, ErrSeatUnavailable)
}

func TestPurchaseAfterReservationTTL(t *testing.T) {
	f := newFixture(t, newSvc(30*time.Millisecond, time.Minute), 0, 2000)
	u := f.user(t, 10000)
	r := f.reserve(t, u, "A1")
	time.Sleep(60 * time.Millisecond)
	_, err := f.svc.StartPurchase(ctx, r.ID, u.ID)
	wantErr(t, err, ErrReservationExpired)
	eq(t, "seat", f.rawSeatStatus(t, "A1"), SeatAvailable)
}

func TestSuccessfulPayment(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, time.Minute), 50000, 2000)
	u := f.user(t, 10000)
	p := f.startPurchase(t, u, "A1")
	ub, ob := f.balances(t, u)
	eq(t, "user before", ub, 10000)
	eq(t, "org before", ob, 50000)

	pay, err := f.svc.Confirm(ctx, p.PaymentID)
	must(t, err)
	eq(t, "payment", pay.Status, PaySucceeded)
	eq(t, "purchase", pay.PurchaseStatus, PurchasePurchase)
	ub, ob = f.balances(t, u)
	eq(t, "user after", ub, 8000)
	eq(t, "org after", ob, 52000)
	eq(t, "seat", f.seatStatus(t, "A1"), SeatPurchased)
	pu, err := f.svc.GetPurchase(ctx, p.PurchaseID)
	must(t, err)
	eq(t, "purchase", pu.Status, PurchasePurchase)

	_, err = f.svc.Reserve(ctx, u.ID, f.event.ID, "A1")
	wantErr(t, err, ErrSeatUnavailable)
	hist, err := f.svc.Transitions(ctx, p.PaymentID)
	must(t, err)
	eq(t, "history length", len(hist), 2) // created, succeeded
}

func TestInsufficientFunds(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, time.Minute), 50000, 2000)
	u := f.user(t, 500)
	p := f.startPurchase(t, u, "A1")
	pay, err := f.svc.Confirm(ctx, p.PaymentID)
	wantErr(t, err, ErrInsufficientFunds)
	eq(t, "payment", pay.Status, PayFailed)
	eq(t, "reason", pay.FailureReason, "insufficient_funds")
	eq(t, "purchase", pay.PurchaseStatus, PurchaseFailed)
	ub, ob := f.balances(t, u)
	eq(t, "user", ub, 500)
	eq(t, "org", ob, 50000)
	eq(t, "seat", f.seatStatus(t, "A1"), SeatAvailable)
	// the state was persisted, not rolled back
	got, _ := f.svc.GetPayment(ctx, p.PaymentID)
	eq(t, "persisted", got.Status, PayFailed)
}

func TestFailEndpoint(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, time.Minute), 50000, 2000)
	u := f.user(t, 10000)
	p := f.startPurchase(t, u, "A1")
	pay, err := f.svc.Fail(ctx, p.PaymentID)
	must(t, err)
	eq(t, "payment", pay.Status, PayFailed)
	eq(t, "purchase", pay.PurchaseStatus, PurchaseFailed)
	ub, ob := f.balances(t, u)
	eq(t, "user", ub, 10000)
	eq(t, "org", ob, 50000)
	eq(t, "seat", f.seatStatus(t, "A1"), SeatAvailable)
	_, err = f.svc.Confirm(ctx, p.PaymentID)
	wantErr(t, err, ErrPaymentNotPending)
}

func TestPaymentTimeout(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, 50*time.Millisecond), 50000, 2000)
	u := f.user(t, 10000)
	p := f.startPurchase(t, u, "A1")
	time.Sleep(80 * time.Millisecond)
	got, _ := f.svc.GetPayment(ctx, p.PaymentID)
	eq(t, "effective payment status", got.Status, PayTimedOut)
	n, err := f.svc.TimeoutDuePayments(ctx)
	must(t, err)
	if n < 1 {
		t.Fatalf("worker timed out %d payments", n)
	}
	got, _ = f.svc.GetPayment(ctx, p.PaymentID)
	eq(t, "payment", got.Status, PayTimedOut)
	eq(t, "purchase", got.PurchaseStatus, PurchaseTimedOut)
	eq(t, "seat", f.rawSeatStatus(t, "A1"), SeatAvailable)
	ub, ob := f.balances(t, u)
	eq(t, "user", ub, 10000)
	eq(t, "org", ob, 50000)
}

// A reservation TTL that has passed does not matter once payment is pending.
func TestDelayedConfirm(t *testing.T) {
	f := newFixture(t, newSvc(150*time.Millisecond, time.Minute), 0, 2000)
	u := f.user(t, 10000)
	r := f.reserve(t, u, "A1")
	p, err := f.svc.StartPurchase(ctx, r.ID, u.ID)
	must(t, err)
	time.Sleep(250 * time.Millisecond) // reservation TTL is long over
	_, err = f.svc.ExpireDueReservations(ctx)
	must(t, err)
	eq(t, "seat still owned by purchase", f.seatStatus(t, "A1"), SeatPaymentPending)
	pay, err := f.svc.Confirm(ctx, p.PaymentID)
	must(t, err)
	eq(t, "payment", pay.Status, PaySucceeded)
	eq(t, "seat", f.seatStatus(t, "A1"), SeatPurchased)
}

func TestLateConfirm(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, 50*time.Millisecond), 50000, 2000)
	u1, u2 := f.user(t, 10000), f.user(t, 10000)
	p := f.startPurchase(t, u1, "A1")
	time.Sleep(80 * time.Millisecond)
	_, err := f.svc.TimeoutDuePayments(ctx)
	must(t, err)
	f.reserve(t, u2, "A1") // user 2 takes the freed seat

	pay, err := f.svc.Confirm(ctx, p.PaymentID)
	wantErr(t, err, ErrPaymentTimedOut)
	eq(t, "payment", pay.Status, PayTimedOut)
	ub, ob := f.balances(t, u1)
	eq(t, "user1", ub, 10000)
	eq(t, "org", ob, 50000)
	eq(t, "seat stays with user 2", f.seatStatus(t, "A1"), SeatReserved)
}

// Even without the worker, a confirm after the deadline is rejected.
func TestLateConfirmWithoutWorker(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, 40*time.Millisecond), 50000, 2000)
	u := f.user(t, 10000)
	p := f.startPurchase(t, u, "A1")
	time.Sleep(70 * time.Millisecond)
	_, err := f.svc.Confirm(ctx, p.PaymentID)
	wantErr(t, err, ErrPaymentTimedOut)
	ub, ob := f.balances(t, u)
	eq(t, "user", ub, 10000)
	eq(t, "org", ob, 50000)
	eq(t, "seat", f.rawSeatStatus(t, "A1"), SeatAvailable)
}

func TestDuplicateConfirm(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, time.Minute), 50000, 2000)
	u := f.user(t, 10000)
	p := f.startPurchase(t, u, "A1")
	for i := 0; i < 3; i++ {
		pay, err := f.svc.Confirm(ctx, p.PaymentID)
		must(t, err)
		eq(t, "payment", pay.Status, PaySucceeded)
	}
	ub, ob := f.balances(t, u)
	eq(t, "user", ub, 8000)
	eq(t, "org", ob, 52000)

	// and concurrently
	p2 := f.startPurchaseOther(t, f.user(t, 10000))
	parallel(20, func(int) {
		if _, err := f.svc.Confirm(ctx, p2.PaymentID); err != nil {
			t.Error(err)
		}
	})
	_, ob = f.balances(t, u)
	eq(t, "org after concurrent duplicates", ob, 54000)
}

func (f *fixture) startPurchaseOther(t *testing.T, u *User) *PurchaseStarted {
	t.Helper()
	// a dedicated seat for this user
	seatNo := newID("S")
	_, err := testPool.Exec(ctx, `INSERT INTO seats (event_id, number, ord, price) VALUES ($1,$2,1000,2000)`, f.event.ID, seatNo)
	must(t, err)
	return f.startPurchase(t, u, seatNo)
}

// Experiment 2: one balance, two payments.
func TestDoubleSpending(t *testing.T) {
	for round := 0; round < 20; round++ {
		f := newFixture(t, newSvc(time.Minute, time.Minute), 0, 2000, 2000)
		u := f.user(t, 2000)
		pa := f.startPurchase(t, u, "A1")
		pb := f.startPurchase(t, u, "A2")
		var ok, insufficient atomic.Int32
		parallel(2, func(i int) {
			id := []string{pa.PaymentID, pb.PaymentID}[i]
			_, err := f.svc.Confirm(ctx, id)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrInsufficientFunds):
				insufficient.Add(1)
			default:
				t.Error(err)
			}
		})
		eq(t, "succeeded", ok.Load(), 1)
		eq(t, "insufficient", insufficient.Load(), 1)
		ub, ob := f.balances(t, u)
		eq(t, "user", ub, 0)
		eq(t, "org", ob, 2000)
	}
}

// Many payments against one balance: it never goes negative, exactly balance/price succeed.
func TestBalanceHotspot(t *testing.T) {
	prices := make([]int64, 30)
	for i := range prices {
		prices[i] = 100
	}
	f := newFixture(t, newSvc(time.Minute, time.Minute), 0, prices...)
	u := f.user(t, 1000)
	pays := make([]*PurchaseStarted, 30)
	for i := range pays {
		pays[i] = f.startPurchase(t, u, fmt.Sprintf("A%d", i+1))
	}
	var ok atomic.Int32
	parallel(30, func(i int) {
		if _, err := f.svc.Confirm(ctx, pays[i].PaymentID); err == nil {
			ok.Add(1)
		} else if !errors.Is(err, ErrInsufficientFunds) {
			t.Error(err)
		}
	})
	eq(t, "succeeded", ok.Load(), 10)
	ub, ob := f.balances(t, u)
	eq(t, "user", ub, 0)
	eq(t, "org", ob, 1000)
}

func TestCancelVsPurchase(t *testing.T) {
	for i := 0; i < 40; i++ {
		f := newFixture(t, newSvc(time.Minute, time.Minute), 0, 2000)
		u := f.user(t, 10000)
		r := f.reserve(t, u, "A1")
		var cancelErr, purchaseErr error
		parallel(2, func(k int) {
			if k == 0 {
				_, cancelErr = f.svc.Cancel(ctx, r.ID)
			} else {
				_, purchaseErr = f.svc.StartPurchase(ctx, r.ID, u.ID)
			}
		})
		got, _ := f.svc.GetReservation(ctx, r.ID)
		switch got.Status {
		case ResCanceled:
			must(t, cancelErr)
			wantErr(t, purchaseErr, ErrInvalidState)
			eq(t, "seat", f.rawSeatStatus(t, "A1"), SeatAvailable)
		case ResPaymentPending:
			must(t, purchaseErr)
			wantErr(t, cancelErr, ErrInvalidState)
			eq(t, "seat", f.rawSeatStatus(t, "A1"), SeatPaymentPending)
		default:
			t.Fatalf("unexpected status %s", got.Status)
		}
	}
}

// Experiment 3: expire vs purchase right at the TTL boundary.
func TestExpireVsPurchase(t *testing.T) {
	var expired, pending int
	for i := 0; i < 60; i++ {
		f := newFixture(t, newSvc(25*time.Millisecond, time.Minute), 0, 2000)
		u := f.user(t, 10000)
		r := f.reserve(t, u, "A1")
		// land the race on the boundary, jittered around it
		time.Sleep(time.Until(r.ExpiresAt) - time.Duration(i%6)*time.Millisecond)
		var purchaseErr error
		parallel(2, func(k int) {
			if k == 0 {
				_, _ = f.svc.ExpireReservation(ctx, r.ID)
			} else {
				_, purchaseErr = f.svc.StartPurchase(ctx, r.ID, u.ID)
			}
		})
		got, _ := f.svc.GetReservation(ctx, r.ID)
		switch got.Status {
		case ResExpired:
			expired++
			if purchaseErr == nil {
				t.Fatal("purchase succeeded but reservation is expired")
			}
			eq(t, "seat", f.rawSeatStatus(t, "A1"), SeatAvailable)
		case ResPaymentPending:
			pending++
			must(t, purchaseErr)
			eq(t, "seat", f.rawSeatStatus(t, "A1"), SeatPaymentPending)
			var n int
			must(t, testPool.QueryRow(ctx, `SELECT count(*) FROM purchases WHERE reservation_id=$1`, r.ID).Scan(&n))
			eq(t, "purchases", n, 1)
		default:
			t.Fatalf("unexpected status %s", got.Status)
		}
	}
	t.Logf("expire-vs-purchase: expired=%d payment_pending=%d", expired, pending)
}

// Experiment 4: timeout vs confirm at the payment deadline.
func TestTimeoutVsConfirm(t *testing.T) {
	var timedOut, purchased int
	for i := 0; i < 60; i++ {
		f := newFixture(t, newSvc(time.Minute, 40*time.Millisecond), 50000, 2000)
		u := f.user(t, 10000)
		p := f.startPurchase(t, u, "A1")
		time.Sleep(time.Until(p.PaymentExpiresAt) - time.Duration(i%6)*time.Millisecond)
		parallel(2, func(k int) {
			if k == 0 {
				_, _ = f.svc.TimeoutPayment(ctx, p.PaymentID)
			} else {
				_, _ = f.svc.Confirm(ctx, p.PaymentID)
			}
		})
		pay, _ := f.svc.GetPayment(ctx, p.PaymentID)
		ub, ob := f.balances(t, u)
		switch pay.Status {
		case PaySucceeded:
			purchased++
			eq(t, "purchase", pay.PurchaseStatus, PurchasePurchase)
			eq(t, "user", ub, 8000)
			eq(t, "org", ob, 52000)
			eq(t, "seat", f.rawSeatStatus(t, "A1"), SeatPurchased)
		case PayTimedOut:
			timedOut++
			eq(t, "purchase", pay.PurchaseStatus, PurchaseTimedOut)
			eq(t, "user", ub, 10000)
			eq(t, "org", ob, 50000)
			eq(t, "seat", f.rawSeatStatus(t, "A1"), SeatAvailable)
		default:
			t.Fatalf("unexpected payment status %s", pay.Status)
		}
	}
	t.Logf("timeout-vs-confirm: purchased=%d timed_out=%d", purchased, timedOut)
}

// After a restart (new Service on the same DB, no worker pass) expired state is not "active".
func TestRestart(t *testing.T) {
	svc := newSvc(30*time.Millisecond, 40*time.Millisecond)
	f := newFixture(t, svc, 50000, 2000, 2000)
	u := f.user(t, 10000)
	f.reserve(t, u, "A1")
	p := f.startPurchase(t, u, "A2")
	time.Sleep(80 * time.Millisecond)

	restarted := New(testPool, Config{ReservationTTL: time.Minute, PaymentTimeout: time.Minute})
	f.svc = restarted
	eq(t, "A1", f.seatStatus(t, "A1"), SeatAvailable)
	eq(t, "A2 (payment_pending past deadline)", f.seatStatus(t, "A2"), SeatPaymentPending)
	_, err := restarted.Confirm(ctx, p.PaymentID)
	wantErr(t, err, ErrPaymentTimedOut)
	f.reserve(t, f.user(t, 0), "A1")
}

func TestWorkersStopOnContext(t *testing.T) {
	svc := newSvc(20*time.Millisecond, 20*time.Millisecond)
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		svc.RunWorkers(wctx, 10*time.Millisecond, t.Logf)
		close(done)
	}()
	f := newFixture(t, svc, 0, 2000)
	r := f.reserve(t, f.user(t, 0), "A1")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && f.rawSeatStatus(t, "A1") != SeatAvailable {
		time.Sleep(10 * time.Millisecond)
	}
	eq(t, "worker freed the seat", f.rawSeatStatus(t, "A1"), SeatAvailable)
	got, _ := svc.GetReservation(ctx, r.ID)
	eq(t, "reservation", got.Status, ResExpired)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
}

// A cancelled request context must not leave partial state.
func TestCancelledContextRollsBack(t *testing.T) {
	f := newFixture(t, newSvc(time.Minute, time.Minute), 50000, 2000)
	u := f.user(t, 10000)
	p := f.startPurchase(t, u, "A1")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := f.svc.Confirm(cctx, p.PaymentID)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	ub, ob := f.balances(t, u)
	eq(t, "user", ub, 10000)
	eq(t, "org", ob, 50000)
	got, _ := f.svc.GetPayment(ctx, p.PaymentID)
	eq(t, "payment", got.Status, PayPending)
}
