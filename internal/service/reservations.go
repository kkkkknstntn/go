package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const effectiveResStatus = `CASE WHEN status = 'reserved' AND expires_at <= clock_timestamp() THEN 'expired' ELSE status END`

func loadReservation(ctx context.Context, q querier, id string) (*Reservation, error) {
	var r Reservation
	err := q.QueryRow(ctx, `SELECT id, user_id, event_id, seat_number, `+effectiveResStatus+`,
		created_at, expires_at, finished_at FROM reservations WHERE id=$1`, id).
		Scan(&r.ID, &r.UserID, &r.EventID, &r.SeatID, &r.Status, &r.CreatedAt, &r.ExpiresAt, &r.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: reservation %s", ErrNotFound, id)
	}
	return &r, err
}

func (s *Service) GetReservation(ctx context.Context, id string) (*Reservation, error) {
	return loadReservation(ctx, s.pool, id)
}

// lockSeat is the serialization point of everything that happens to a seat.
func lockSeat(ctx context.Context, tx pgx.Tx, eventID, number string) (price int64, status string, err error) {
	err = tx.QueryRow(ctx, `SELECT price, status FROM seats WHERE event_id=$1 AND number=$2 FOR UPDATE`,
		eventID, number).Scan(&price, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("%w: seat %s in event %s", ErrNotFound, number, eventID)
	}
	return
}

func setSeat(ctx context.Context, tx pgx.Tx, eventID, number, status string) error {
	_, err := tx.Exec(ctx, `UPDATE seats SET status=$3 WHERE event_id=$1 AND number=$2`, eventID, number, status)
	return err
}

type resRow struct {
	userID, eventID, seat, status string
	expired                       bool
}

// lockReservation must be called after lockSeat for the reservation's seat.
func lockReservation(ctx context.Context, tx pgx.Tx, id string) (*resRow, error) {
	var r resRow
	err := tx.QueryRow(ctx, `SELECT user_id, event_id, seat_number, status, expires_at <= clock_timestamp()
		FROM reservations WHERE id=$1 FOR UPDATE`, id).
		Scan(&r.userID, &r.eventID, &r.seat, &r.status, &r.expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: reservation %s", ErrNotFound, id)
	}
	return &r, err
}

// seatOfReservation reads the (immutable) seat of a reservation without locking,
// so the caller can take the seat lock first.
func seatOfReservation(ctx context.Context, q querier, id string) (eventID, seat string, err error) {
	err = q.QueryRow(ctx, `SELECT event_id, seat_number FROM reservations WHERE id=$1`, id).Scan(&eventID, &seat)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("%w: reservation %s", ErrNotFound, id)
	}
	return
}

// closeReservation moves a locked reservation to a terminal status and frees the seat.
func closeReservation(ctx context.Context, tx pgx.Tx, id string, r *resRow, to, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE reservations SET status=$2, finished_at=clock_timestamp() WHERE id=$1`, id, to); err != nil {
		return err
	}
	if err := logTransition(ctx, tx, "reservation", id, r.status, to, reason); err != nil {
		return err
	}
	return setSeat(ctx, tx, r.eventID, r.seat, SeatAvailable)
}

// Reserve atomically claims a seat. The decision "this seat now belongs to this
// user" is taken under the row lock of the seat, in the same transaction that
// inserts the reservation.
func (s *Service) Reserve(ctx context.Context, userID, eventID, number string) (*Reservation, error) {
	if userID == "" || eventID == "" || number == "" {
		return nil, fmt.Errorf("%w: userId, eventId and seatId are required", ErrBadRequest)
	}
	var out *Reservation
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id=$1)`, userID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: user %s", ErrNotFound, userID)
		}
		_, status, err := lockSeat(ctx, tx, eventID, number)
		if err != nil {
			return err
		}
		if status == SeatReserved {
			// Lazy expiration: a stale reservation must not block the seat
			// even if the background worker is late.
			var id string
			err := tx.QueryRow(ctx, `SELECT id FROM reservations
				WHERE event_id=$1 AND seat_number=$2 AND status='reserved' AND expires_at <= clock_timestamp()
				FOR UPDATE`, eventID, number).Scan(&id)
			if err == nil {
				if err := closeReservation(ctx, tx, id, &resRow{eventID: eventID, seat: number, status: ResReserved}, ResExpired, "ttl (lazy)"); err != nil {
					return err
				}
				status = SeatAvailable
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if status != SeatAvailable {
			return fmt.Errorf("%w: %s is %s", ErrSeatUnavailable, number, status)
		}
		id := newID("r-")
		var res Reservation
		err = tx.QueryRow(ctx, `INSERT INTO reservations (id, user_id, event_id, seat_number, status, expires_at)
			VALUES ($1,$2,$3,$4,'reserved', clock_timestamp() + make_interval(secs => $5))
			RETURNING id, user_id, event_id, seat_number, status, created_at, expires_at, finished_at`,
			id, userID, eventID, number, s.cfg.ReservationTTL.Seconds()).
			Scan(&res.ID, &res.UserID, &res.EventID, &res.SeatID, &res.Status, &res.CreatedAt, &res.ExpiresAt, &res.FinishedAt)
		if err != nil {
			return err
		}
		if err := setSeat(ctx, tx, eventID, number, SeatReserved); err != nil {
			return err
		}
		if err := logTransition(ctx, tx, "reservation", id, "", ResReserved, ""); err != nil {
			return err
		}
		out = &res
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Cancel moves reserved -> canceled and frees the seat.
func (s *Service) Cancel(ctx context.Context, id string) (*Reservation, error) {
	var postErr error
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		eventID, number, err := seatOfReservation(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, _, err := lockSeat(ctx, tx, eventID, number); err != nil {
			return err
		}
		r, err := lockReservation(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case r.status != ResReserved:
			postErr = fmt.Errorf("%w: cannot cancel a %s reservation", ErrInvalidState, r.status)
		case r.expired:
			postErr = fmt.Errorf("%w: ttl is over", ErrReservationExpired)
			return closeReservation(ctx, tx, id, r, ResExpired, "ttl (on cancel)")
		default:
			return closeReservation(ctx, tx, id, r, ResCanceled, "user cancel")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if postErr != nil {
		return nil, postErr
	}
	return s.GetReservation(ctx, id)
}

// ExpireReservation expires one reservation if it is still reserved and past its
// TTL. It reports whether it did. Safe to call concurrently from any instance.
func (s *Service) ExpireReservation(ctx context.Context, id string) (bool, error) {
	expired := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		eventID, number, err := seatOfReservation(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, _, err := lockSeat(ctx, tx, eventID, number); err != nil {
			return err
		}
		r, err := lockReservation(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.status != ResReserved || !r.expired {
			return nil
		}
		expired = true
		return closeReservation(ctx, tx, id, r, ResExpired, "ttl (worker)")
	})
	return expired, err
}

// ExpireDueReservations is one pass of the expiration worker.
func (s *Service) ExpireDueReservations(ctx context.Context) (int, error) {
	ids, err := s.dueIDs(ctx, `SELECT id FROM reservations WHERE status='reserved' AND expires_at <= clock_timestamp() ORDER BY expires_at LIMIT 500`)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		ok, err := s.ExpireReservation(ctx, id)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

func (s *Service) dueIDs(ctx context.Context, query string) ([]string, error) {
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RunWorkers runs expiration and payment-timeout passes until ctx is cancelled.
func (s *Service) RunWorkers(ctx context.Context, interval time.Duration, logf func(format string, args ...any)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := s.ExpireDueReservations(ctx); err != nil && ctx.Err() == nil {
			logf("reservation expiration worker: %v", err)
		} else if n > 0 {
			logf("expired %d reservations", n)
		}
		if n, err := s.TimeoutDuePayments(ctx); err != nil && ctx.Err() == nil {
			logf("payment timeout worker: %v", err)
		} else if n > 0 {
			logf("timed out %d payments", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
