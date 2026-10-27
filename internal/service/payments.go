package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// payment status as seen by clients: a pending payment past its deadline is
// reported as timed_out even before the worker has processed it.
const loadPaymentSQL = `
	SELECT p.id, p.purchase_id, p.user_id, p.organizer_id, p.amount,
	       CASE WHEN p.status = 'pending' AND p.expires_at <= clock_timestamp() THEN 'timed_out' ELSE p.status END,
	       COALESCE(p.failure_reason, ''),
	       CASE WHEN pu.status = 'payment_pending' AND p.expires_at <= clock_timestamp() THEN 'timed_out' ELSE pu.status END,
	       p.created_at, p.expires_at, p.completed_at
	FROM payments p JOIN purchases pu ON pu.id = p.purchase_id
	WHERE p.id = $1`

func loadPayment(ctx context.Context, q querier, id string) (*Payment, error) {
	var p Payment
	err := q.QueryRow(ctx, loadPaymentSQL, id).Scan(&p.ID, &p.PurchaseID, &p.UserID, &p.OrganizerID, &p.Amount,
		&p.Status, &p.FailureReason, &p.PurchaseStatus, &p.CreatedAt, &p.ExpiresAt, &p.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: payment %s", ErrNotFound, id)
	}
	return &p, err
}

func (s *Service) GetPayment(ctx context.Context, id string) (*Payment, error) {
	return loadPayment(ctx, s.pool, id)
}

func (s *Service) GetPurchase(ctx context.Context, id string) (*Purchase, error) {
	var p Purchase
	err := s.pool.QueryRow(ctx, `
		SELECT pu.id, pu.reservation_id, pay.id,
		       CASE WHEN pu.status = 'payment_pending' AND pay.expires_at <= clock_timestamp() THEN 'timed_out' ELSE pu.status END,
		       pu.started_at, pu.finished_at
		FROM purchases pu JOIN payments pay ON pay.purchase_id = pu.id
		WHERE pu.id = $1`, id).
		Scan(&p.ID, &p.ReservationID, &p.PaymentID, &p.Status, &p.StartedAt, &p.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: purchase %s", ErrNotFound, id)
	}
	return &p, err
}

// StartPurchase moves reserved -> payment_pending and creates Purchase and Payment.
// From this moment the reservation TTL no longer applies; the payment timeout does.
func (s *Service) StartPurchase(ctx context.Context, reservationID, userID string) (*PurchaseStarted, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: userId is required", ErrBadRequest)
	}
	var out *PurchaseStarted
	var postErr error
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		eventID, number, err := seatOfReservation(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		price, _, err := lockSeat(ctx, tx, eventID, number)
		if err != nil {
			return err
		}
		r, err := lockReservation(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		if r.userID != userID {
			return ErrForbidden
		}
		if r.status != ResReserved {
			return fmt.Errorf("%w: reservation is %s", ErrInvalidState, r.status)
		}
		if r.expired {
			postErr = fmt.Errorf("%w: ttl is over", ErrReservationExpired)
			return closeReservation(ctx, tx, reservationID, r, ResExpired, "ttl (on purchase)")
		}
		var organizerID string
		if err := tx.QueryRow(ctx, `SELECT organizer_id FROM events WHERE id=$1`, eventID).Scan(&organizerID); err != nil {
			return err
		}
		purchaseID, paymentID := newID("purchase-"), newID("payment-")
		if _, err := tx.Exec(ctx, `INSERT INTO purchases (id, reservation_id, status) VALUES ($1,$2,'payment_pending')`,
			purchaseID, reservationID); err != nil {
			return err
		}
		var res PurchaseStarted
		err = tx.QueryRow(ctx, `INSERT INTO payments (id, purchase_id, user_id, organizer_id, amount, status, expires_at)
			VALUES ($1,$2,$3,$4,$5,'pending', clock_timestamp() + make_interval(secs => $6))
			RETURNING id, purchase_id, status, amount, expires_at`,
			paymentID, purchaseID, userID, organizerID, price, s.cfg.PaymentTimeout.Seconds()).
			Scan(&res.PaymentID, &res.PurchaseID, &res.Status, &res.Amount, &res.PaymentExpiresAt)
		if err != nil {
			return err
		}
		res.Status = PurchasePending
		if _, err := tx.Exec(ctx, `UPDATE reservations SET status='payment_pending' WHERE id=$1`, reservationID); err != nil {
			return err
		}
		if err := setSeat(ctx, tx, eventID, number, SeatPaymentPending); err != nil {
			return err
		}
		for _, l := range [][4]string{
			{"reservation", reservationID, ResReserved, ResPaymentPending},
			{"purchase", purchaseID, "", PurchasePending},
			{"payment", paymentID, "", PayPending},
		} {
			if err := logTransition(ctx, tx, l[0], l[1], l[2], l[3], ""); err != nil {
				return err
			}
		}
		out = &res
		return nil
	})
	if err != nil {
		return nil, err
	}
	if postErr != nil {
		return nil, postErr
	}
	return out, nil
}

type payRef struct {
	paymentID, purchaseID, reservationID, eventID, seat string
	userID, organizerID, status                         string
	amount                                              int64
	expired                                             bool
}

// lockPayment takes the seat lock first (the common serialization point), then
// the payment row. The seat of a payment never changes, so it is read unlocked.
func lockPayment(ctx context.Context, tx pgx.Tx, paymentID string) (*payRef, error) {
	p := &payRef{paymentID: paymentID}
	err := tx.QueryRow(ctx, `
		SELECT p.purchase_id, pu.reservation_id, r.event_id, r.seat_number
		FROM payments p
		JOIN purchases pu ON pu.id = p.purchase_id
		JOIN reservations r ON r.id = pu.reservation_id
		WHERE p.id = $1`, paymentID).Scan(&p.purchaseID, &p.reservationID, &p.eventID, &p.seat)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: payment %s", ErrNotFound, paymentID)
	}
	if err != nil {
		return nil, err
	}
	if _, _, err := lockSeat(ctx, tx, p.eventID, p.seat); err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `SELECT user_id, organizer_id, amount, status, expires_at <= clock_timestamp()
		FROM payments WHERE id=$1 FOR UPDATE`, paymentID).
		Scan(&p.userID, &p.organizerID, &p.amount, &p.status, &p.expired)
	return p, err
}

// finishPayment closes a pending payment together with its purchase and
// reservation and sets the final seat status, all in the caller's transaction.
func finishPayment(ctx context.Context, tx pgx.Tx, p *payRef, payStatus, purchaseStatus, resStatus, seatStatus, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE payments SET status=$2, failure_reason=NULLIF($3,''), completed_at=clock_timestamp() WHERE id=$1`,
		p.paymentID, payStatus, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE purchases SET status=$2, finished_at=clock_timestamp() WHERE id=$1`, p.purchaseID, purchaseStatus); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE reservations SET status=$2, finished_at=clock_timestamp() WHERE id=$1`, p.reservationID, resStatus); err != nil {
		return err
	}
	if err := setSeat(ctx, tx, p.eventID, p.seat, seatStatus); err != nil {
		return err
	}
	for _, l := range [][4]string{
		{"payment", p.paymentID, PayPending, payStatus},
		{"purchase", p.purchaseID, PurchasePending, purchaseStatus},
		{"reservation", p.reservationID, ResPaymentPending, resStatus},
	} {
		if err := logTransition(ctx, tx, l[0], l[1], l[2], l[3], reason); err != nil {
			return err
		}
	}
	return nil
}

func timeoutPayment(ctx context.Context, tx pgx.Tx, p *payRef, reason string) error {
	return finishPayment(ctx, tx, p, PayTimedOut, PurchaseTimedOut, ResExpired, SeatAvailable, reason)
}

// Confirm models the payment provider callback. The whole money transfer is one
// transaction: either every step happens or none does.
//
// Outcomes:
//   - pending, in time, enough money -> succeeded (money moves)
//   - succeeded already              -> same succeeded payment (idempotent, no money moves)
//   - pending, in time, no money     -> failed, ErrInsufficientFunds (committed, no money moves)
//   - pending, past deadline         -> timed_out, ErrPaymentTimedOut (no money moves)
//   - failed / timed_out             -> ErrPaymentNotPending / ErrPaymentTimedOut
//
// When an error accompanies a non-nil Payment, the payment state was persisted.
func (s *Service) Confirm(ctx context.Context, paymentID string) (*Payment, error) {
	var out *Payment
	var postErr error
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := lockPayment(ctx, tx, paymentID)
		if err != nil {
			return err
		}
		switch p.status {
		case PaySucceeded:
			// duplicate callback: nothing to do
		case PayFailed:
			postErr = fmt.Errorf("%w: payment already failed", ErrPaymentNotPending)
		case PayTimedOut:
			postErr = fmt.Errorf("%w: confirmation arrived too late", ErrPaymentTimedOut)
		default: // pending
			if p.expired {
				postErr = fmt.Errorf("%w: confirmation arrived too late", ErrPaymentTimedOut)
				if err := timeoutPayment(ctx, tx, p, "timeout (on confirm)"); err != nil {
					return err
				}
				break
			}
			// Row lock on the user serializes all concurrent spenders of one balance;
			// the balance is checked only after the lock is held.
			var balance int64
			if err := tx.QueryRow(ctx, `SELECT balance FROM users WHERE id=$1 FOR UPDATE`, p.userID).Scan(&balance); err != nil {
				return err
			}
			if balance < p.amount {
				postErr = fmt.Errorf("%w: balance %d, price %d", ErrInsufficientFunds, balance, p.amount)
				if err := finishPayment(ctx, tx, p, PayFailed, PurchaseFailed, ResPaymentFailed, SeatAvailable, "insufficient_funds"); err != nil {
					return err
				}
				break
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET balance = balance - $2 WHERE id=$1`, p.userID, p.amount); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE organizers SET balance = balance + $2 WHERE id=$1`, p.organizerID, p.amount); err != nil {
				return err
			}
			if err := finishPayment(ctx, tx, p, PaySucceeded, PurchasePurchase, ResPurchased, SeatPurchased, ""); err != nil {
				return err
			}
		}
		out, err = loadPayment(ctx, tx, paymentID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, postErr
}

// Fail models a refusal by the payment provider: no money moves, the seat is freed.
func (s *Service) Fail(ctx context.Context, paymentID string) (*Payment, error) {
	var out *Payment
	var postErr error
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := lockPayment(ctx, tx, paymentID)
		if err != nil {
			return err
		}
		switch p.status {
		case PayFailed:
			// idempotent
		case PaySucceeded:
			postErr = fmt.Errorf("%w: payment already succeeded", ErrPaymentNotPending)
		case PayTimedOut:
			postErr = fmt.Errorf("%w: payment already timed out", ErrPaymentNotPending)
		default:
			if p.expired {
				postErr = fmt.Errorf("%w: payment deadline passed", ErrPaymentTimedOut)
				if err := timeoutPayment(ctx, tx, p, "timeout (on fail)"); err != nil {
					return err
				}
				break
			}
			if err := finishPayment(ctx, tx, p, PayFailed, PurchaseFailed, ResPaymentFailed, SeatAvailable, "declined"); err != nil {
				return err
			}
		}
		out, err = loadPayment(ctx, tx, paymentID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, postErr
}

// TimeoutPayment times out one payment if it is still pending and past its
// deadline. It reports whether it did.
func (s *Service) TimeoutPayment(ctx context.Context, paymentID string) (bool, error) {
	done := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := lockPayment(ctx, tx, paymentID)
		if err != nil {
			return err
		}
		if p.status != PayPending || !p.expired {
			return nil
		}
		done = true
		return timeoutPayment(ctx, tx, p, "timeout (worker)")
	})
	return done, err
}

// TimeoutDuePayments is one pass of the payment timeout worker.
func (s *Service) TimeoutDuePayments(ctx context.Context) (int, error) {
	ids, err := s.dueIDs(ctx, `SELECT id FROM payments WHERE status='pending' AND expires_at <= clock_timestamp() ORDER BY expires_at LIMIT 500`)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		ok, err := s.TimeoutPayment(ctx, id)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}
