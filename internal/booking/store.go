package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/postgres"
)

// store holds the SQL of the package: this file has the locking queries and
// status changes, store_read.go the queries for clients, store_catalog.go
// organizers, events and users.
//
// The write methods expect the caller to hold the lock of the seat involved.
// Deadlines are compared with clock_timestamp() in queries issued after that
// lock was taken, so they reflect the moment of the decision.
type store struct {
	q postgres.Querier
}

type seatRow struct {
	EventID string
	Number  string
	Price   int64
	Status  domain.SeatStatus
}

func (r seatRow) key() string { return r.EventID + "/" + r.Number }

// lockSeat serializes everything that happens to one seat.
func (s store) lockSeat(ctx context.Context, eventID, number string) (seatRow, error) {
	row := seatRow{EventID: eventID, Number: number}
	err := s.q.QueryRow(ctx,
		`SELECT price, status FROM seats WHERE event_id = $1 AND number = $2 FOR UPDATE`,
		eventID, number).Scan(&row.Price, &row.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, fmt.Errorf("%w: seat %s of event %s", domain.ErrNotFound, number, eventID)
	}
	return row, err
}

func (s store) moveSeat(ctx context.Context, seat *seatRow, to domain.SeatStatus, reason string) error {
	from := seat.Status
	if !from.CanTransitionTo(to) {
		return invalidTransition("seat", seat.key(), from, to)
	}
	tag, err := s.q.Exec(ctx,
		`UPDATE seats SET status = $4 WHERE event_id = $1 AND number = $2 AND status = $3`,
		seat.EventID, seat.Number, from, to)
	if err := checkApplied(tag, err, "seat", seat.key()); err != nil {
		return err
	}
	seat.Status = to
	return s.logTransition(ctx, "seat", seat.key(), string(from), string(to), reason)
}

type reservationRow struct {
	ID         string
	UserID     string
	EventID    string
	SeatNumber string
	Status     domain.ReservationStatus
	Expired    bool
}

// reservationSeat reads the seat of a reservation without locking. The seat
// never changes, so the caller can lock it before locking the reservation.
func (s store) reservationSeat(ctx context.Context, id string) (eventID, number string, err error) {
	err = s.q.QueryRow(ctx, `SELECT event_id, seat_number FROM reservations WHERE id = $1`, id).Scan(&eventID, &number)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("%w: reservation %s", domain.ErrNotFound, id)
	}
	return eventID, number, err
}

func (s store) lockReservation(ctx context.Context, id string) (reservationRow, error) {
	r := reservationRow{ID: id}
	err := s.q.QueryRow(ctx, `
		SELECT user_id, event_id, seat_number, status, expires_at <= clock_timestamp()
		FROM reservations WHERE id = $1 FOR UPDATE`, id).
		Scan(&r.UserID, &r.EventID, &r.SeatNumber, &r.Status, &r.Expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: reservation %s", domain.ErrNotFound, id)
	}
	return r, err
}

// lockActiveReservation locks the reservation holding the seat in the
// reserved state, if there is one.
func (s store) lockActiveReservation(ctx context.Context, eventID, number string) (reservationRow, bool, error) {
	r := reservationRow{EventID: eventID, SeatNumber: number}
	err := s.q.QueryRow(ctx, `
		SELECT id, user_id, status, expires_at <= clock_timestamp()
		FROM reservations
		WHERE event_id = $1 AND seat_number = $2 AND status = 'reserved'
		FOR UPDATE`, eventID, number).
		Scan(&r.ID, &r.UserID, &r.Status, &r.Expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

func (s store) insertReservation(ctx context.Context, id, userID string, seat seatRow, ttl time.Duration) (*domain.Reservation, error) {
	var r domain.Reservation
	err := s.q.QueryRow(ctx, `
		INSERT INTO reservations (id, user_id, event_id, seat_number, status, expires_at)
		VALUES ($1, $2, $3, $4, 'reserved', clock_timestamp() + make_interval(secs => $5))
		RETURNING id, user_id, event_id, seat_number, status, created_at, expires_at`,
		id, userID, seat.EventID, seat.Number, ttl.Seconds()).
		Scan(&r.ID, &r.UserID, &r.EventID, &r.SeatID, &r.Status, &r.CreatedAt, &r.ExpiresAt)
	if postgres.IsUniqueViolation(err) { // reservations_one_live_per_seat
		return nil, fmt.Errorf("%w: seat %s already has a live reservation", domain.ErrSeatUnavailable, seat.Number)
	}
	if err != nil {
		return nil, err
	}
	return &r, s.logTransition(ctx, "reservation", id, "", string(domain.ReservationReserved), "")
}

func (s store) moveReservation(ctx context.Context, id string, from, to domain.ReservationStatus, reason string) error {
	if !from.CanTransitionTo(to) {
		return invalidTransition("reservation", id, from, to)
	}
	tag, err := s.q.Exec(ctx, `
		UPDATE reservations
		SET status = $3, finished_at = CASE WHEN $4::boolean THEN clock_timestamp() END
		WHERE id = $1 AND status = $2`,
		id, from, to, to.IsFinal())
	if err := checkApplied(tag, err, "reservation", id); err != nil {
		return err
	}
	return s.logTransition(ctx, "reservation", id, string(from), string(to), reason)
}

type paymentRow struct {
	ID            string
	PurchaseID    string
	ReservationID string
	UserID        string
	OrganizerID   string
	Amount        int64
	Status        domain.PaymentStatus
	Expired       bool
}

// paymentSeat reads the seat a payment is for, without locking.
func (s store) paymentSeat(ctx context.Context, paymentID string) (eventID, number string, err error) {
	err = s.q.QueryRow(ctx, `
		SELECT r.event_id, r.seat_number
		FROM payments p
		JOIN purchases pu ON pu.id = p.purchase_id
		JOIN reservations r ON r.id = pu.reservation_id
		WHERE p.id = $1`, paymentID).Scan(&eventID, &number)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("%w: payment %s", domain.ErrNotFound, paymentID)
	}
	return eventID, number, err
}

const selectPaymentForUpdate = `
	SELECT p.id, p.purchase_id, pu.reservation_id, p.user_id, p.organizer_id, p.amount,
	       p.status, p.expires_at <= clock_timestamp()
	FROM payments p
	JOIN purchases pu ON pu.id = p.purchase_id
	JOIN reservations r ON r.id = pu.reservation_id `

func scanPaymentRow(row pgx.Row) (paymentRow, error) {
	var p paymentRow
	err := row.Scan(&p.ID, &p.PurchaseID, &p.ReservationID, &p.UserID, &p.OrganizerID, &p.Amount, &p.Status, &p.Expired)
	return p, err
}

func (s store) lockPayment(ctx context.Context, id string) (paymentRow, error) {
	p, err := scanPaymentRow(s.q.QueryRow(ctx, selectPaymentForUpdate+`WHERE p.id = $1 FOR UPDATE OF p`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("%w: payment %s", domain.ErrNotFound, id)
	}
	return p, err
}

// lockPendingPayment locks the pending payment holding the seat, if there is one.
func (s store) lockPendingPayment(ctx context.Context, eventID, number string) (paymentRow, bool, error) {
	p, err := scanPaymentRow(s.q.QueryRow(ctx,
		selectPaymentForUpdate+`WHERE r.event_id = $1 AND r.seat_number = $2 AND p.status = 'pending' FOR UPDATE OF p`,
		eventID, number))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s store) insertPurchase(ctx context.Context, id, reservationID string) error {
	if _, err := s.q.Exec(ctx,
		`INSERT INTO purchases (id, reservation_id, status) VALUES ($1, $2, 'payment_pending')`,
		id, reservationID); err != nil {
		return err
	}
	return s.logTransition(ctx, "purchase", id, "", string(domain.PurchasePaymentPending), "")
}

func (s store) insertPayment(ctx context.Context, id, purchaseID, userID, organizerID string, amount int64, timeout time.Duration) (time.Time, error) {
	var expiresAt time.Time
	err := s.q.QueryRow(ctx, `
		INSERT INTO payments (id, purchase_id, user_id, organizer_id, amount, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, 'pending', clock_timestamp() + make_interval(secs => $6))
		RETURNING expires_at`,
		id, purchaseID, userID, organizerID, amount, timeout.Seconds()).Scan(&expiresAt)
	if err != nil {
		return time.Time{}, err
	}
	return expiresAt, s.logTransition(ctx, "payment", id, "", string(domain.PaymentPending), "")
}

func (s store) movePurchase(ctx context.Context, id string, from, to domain.PurchaseStatus, reason string) error {
	if !from.CanTransitionTo(to) {
		return invalidTransition("purchase", id, from, to)
	}
	tag, err := s.q.Exec(ctx, `
		UPDATE purchases
		SET status = $3, finished_at = CASE WHEN $4::boolean THEN clock_timestamp() END
		WHERE id = $1 AND status = $2`,
		id, from, to, to.IsFinal())
	if err := checkApplied(tag, err, "purchase", id); err != nil {
		return err
	}
	return s.logTransition(ctx, "purchase", id, string(from), string(to), reason)
}

func (s store) movePayment(ctx context.Context, p *paymentRow, to domain.PaymentStatus, reason string) error {
	from := p.Status
	if !from.CanTransitionTo(to) {
		return invalidTransition("payment", p.ID, from, to)
	}
	failureReason := ""
	if to == domain.PaymentFailed {
		failureReason = reason
	}
	tag, err := s.q.Exec(ctx, `
		UPDATE payments
		SET status = $3, failure_reason = NULLIF($4, ''), completed_at = clock_timestamp()
		WHERE id = $1 AND status = $2`,
		p.ID, from, to, failureReason)
	if err := checkApplied(tag, err, "payment", p.ID); err != nil {
		return err
	}
	p.Status = to
	return s.logTransition(ctx, "payment", p.ID, string(from), string(to), reason)
}

// lockBalance locks the buyer's row. Concurrent payments of one user queue
// here and each sees the balance left by the previous one.
func (s store) lockBalance(ctx context.Context, userID string) (int64, error) {
	var balance int64
	err := s.q.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&balance)
	return balance, err
}

func (s store) transfer(ctx context.Context, userID, organizerID string, amount int64) error {
	// balance >= $2 has already been checked under lockBalance; it stays in
	// the WHERE clause as a last guard against debiting below zero.
	tag, err := s.q.Exec(ctx,
		`UPDATE users SET balance = balance - $2 WHERE id = $1 AND balance >= $2`, userID, amount)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: user %s", domain.ErrInsufficientFunds, userID)
	}
	tag, err = s.q.Exec(ctx, `UPDATE organizers SET balance = balance + $2 WHERE id = $1`, organizerID, amount)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("organizer %s not found", organizerID)
	}
	return nil
}

func (s store) logTransition(ctx context.Context, entity, id, from, to, reason string) error {
	_, err := s.q.Exec(ctx, `
		INSERT INTO transitions (entity, entity_id, from_status, to_status, reason)
		VALUES ($1, $2, $3, $4, $5)`, entity, id, from, to, reason)
	return err
}

func invalidTransition[S ~string](entity, id string, from, to S) error {
	return fmt.Errorf("%w: %s %s cannot go from %s to %s", domain.ErrInvalidTransition, entity, id, from, to)
}

// checkApplied fails if a guarded UPDATE ... WHERE status = <from> changed no
// row. That does not happen while the seat is locked; if it ever does, the
// transaction is rolled back instead of overwriting a status.
func checkApplied(tag pgconn.CommandTag, err error, entity, id string) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s %s is not in the expected state", domain.ErrInvalidTransition, entity, id)
	}
	return nil
}
