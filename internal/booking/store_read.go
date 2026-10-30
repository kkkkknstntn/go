package booking

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"awesomeProject/internal/domain"
)

const (
	effectiveReservationStatus = `CASE
		WHEN r.status = 'reserved' AND r.expires_at <= now() THEN 'expired'
		WHEN r.status = 'payment_pending' AND p.status = 'pending' AND p.expires_at <= now() THEN 'expired'
		ELSE r.status END`
	effectivePurchaseStatus = `CASE
		WHEN pu.status = 'payment_pending' AND p.status = 'pending' AND p.expires_at <= now() THEN 'timed_out'
		ELSE pu.status END`
	effectivePaymentStatus = `CASE
		WHEN p.status = 'pending' AND p.expires_at <= now() THEN 'timed_out'
		ELSE p.status END`
	effectiveSeatStatus = `CASE
		WHEN s.status = 'reserved' AND r.expires_at <= now() THEN 'available'
		WHEN s.status = 'payment_pending' AND p.status = 'pending' AND p.expires_at <= now() THEN 'available'
		ELSE s.status END`
)

func (s store) getReservation(ctx context.Context, id string) (*domain.Reservation, error) {
	var r domain.Reservation
	err := s.q.QueryRow(ctx, `
		SELECT r.id, r.user_id, r.event_id, r.seat_number, `+effectiveReservationStatus+`,
		       r.created_at, r.expires_at, r.finished_at
		FROM reservations r
		LEFT JOIN purchases pu ON pu.reservation_id = r.id
		LEFT JOIN payments p ON p.purchase_id = pu.id
		WHERE r.id = $1`, id).
		Scan(&r.ID, &r.UserID, &r.EventID, &r.SeatID, &r.Status, &r.CreatedAt, &r.ExpiresAt, &r.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: reservation %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s store) getPurchase(ctx context.Context, id string) (*domain.Purchase, error) {
	var pu domain.Purchase
	err := s.q.QueryRow(ctx, `
		SELECT pu.id, pu.reservation_id, p.id, r.user_id, `+effectivePurchaseStatus+`,
		       pu.started_at, pu.finished_at
		FROM purchases pu
		JOIN payments p ON p.purchase_id = pu.id
		JOIN reservations r ON r.id = pu.reservation_id
		WHERE pu.id = $1`, id).
		Scan(&pu.ID, &pu.ReservationID, &pu.PaymentID, &pu.UserID, &pu.Status, &pu.StartedAt, &pu.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: purchase %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return &pu, nil
}

func (s store) getPayment(ctx context.Context, id string) (*domain.Payment, error) {
	var p domain.Payment
	err := s.q.QueryRow(ctx, `
		SELECT p.id, p.purchase_id, pu.reservation_id, p.user_id, p.organizer_id, p.amount,
		       `+effectivePaymentStatus+`, COALESCE(p.failure_reason, ''), `+effectivePurchaseStatus+`,
		       p.created_at, p.expires_at, p.completed_at
		FROM payments p
		JOIN purchases pu ON pu.id = p.purchase_id
		WHERE p.id = $1`, id).
		Scan(&p.ID, &p.PurchaseID, &p.ReservationID, &p.UserID, &p.OrganizerID, &p.Amount,
			&p.Status, &p.FailureReason, &p.PurchaseStatus, &p.CreatedAt, &p.ExpiresAt, &p.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: payment %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s store) listSeats(ctx context.Context, eventID string) ([]domain.Seat, error) {
	var exists bool
	if err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)`, eventID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: event %s", domain.ErrNotFound, eventID)
	}
	rows, err := s.q.Query(ctx, `
		SELECT s.number, s.price, `+effectiveSeatStatus+`
		FROM seats s
		LEFT JOIN reservations r ON r.event_id = s.event_id AND r.seat_number = s.number
		                        AND r.status IN ('reserved', 'payment_pending')
		LEFT JOIN purchases pu ON pu.reservation_id = r.id
		LEFT JOIN payments p ON p.purchase_id = pu.id
		WHERE s.event_id = $1
		ORDER BY s.seat_order`, eventID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Seat, error) {
		var st domain.Seat
		err := row.Scan(&st.Number, &st.Price, &st.Status)
		return st, err
	})
}

func (s store) reservationHistory(ctx context.Context, reservationID string) ([]domain.Transition, error) {
	rows, err := s.q.Query(ctx, `
		SELECT entity, entity_id, from_status, to_status, reason, at
		FROM transitions
		WHERE entity_id IN (
			SELECT $1
			UNION ALL SELECT pu.id FROM purchases pu WHERE pu.reservation_id = $1
			UNION ALL SELECT p.id FROM payments p JOIN purchases pu ON pu.id = p.purchase_id
			          WHERE pu.reservation_id = $1)
		ORDER BY id`, reservationID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Transition, error) {
		var t domain.Transition
		err := row.Scan(&t.Entity, &t.EntityID, &t.From, &t.To, &t.Reason, &t.At)
		return t, err
	})
}
