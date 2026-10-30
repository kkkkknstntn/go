package booking

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/ids"
)

func (s *Service) Reserve(ctx context.Context, userID, eventID, seatNumber string) (*domain.Reservation, error) {
	if err := errors.Join(requireField("userId", userID), requireField("eventId", eventID), requireField("seatId", seatNumber)); err != nil {
		return nil, err
	}
	var out *domain.Reservation
	err := s.inTx(ctx, func(st store) error {
		role, err := st.userRole(ctx, userID)
		if err != nil {
			return err
		}
		if role != domain.RoleUser {
			return fmt.Errorf("%w: only buyers can reserve seats", domain.ErrForbidden)
		}
		seat, err := st.lockSeat(ctx, eventID, seatNumber)
		if err != nil {
			return err
		}
		if err = s.releaseStaleSeat(ctx, st, &seat); err != nil {
			return err
		}
		if seat.Status != domain.SeatAvailable {
			return fmt.Errorf("%w: seat %s is %s", domain.ErrSeatUnavailable, seat.Number, seat.Status)
		}
		out, err = st.insertReservation(ctx, ids.New("r-"), userID, seat, s.cfg.ReservationTTL)
		if err != nil {
			return err
		}
		return st.moveSeat(ctx, &seat, domain.SeatReserved, "reserve "+out.ID)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) releaseStaleSeat(ctx context.Context, st store, seat *seatRow) error {
	switch seat.Status {
	case domain.SeatReserved:
		r, found, err := st.lockActiveReservation(ctx, seat.EventID, seat.Number)
		if err != nil || !found || !r.Expired {
			return err
		}
		return s.expireReservation(ctx, st, seat, r, "reservation ttl (on reserve)")
	case domain.SeatPaymentPending:
		p, found, err := st.lockPendingPayment(ctx, seat.EventID, seat.Number)
		if err != nil || !found || !p.Expired {
			return err
		}
		return s.closePayment(ctx, st, seat, &p, outcomeTimedOut, "payment timeout (on reserve)")
	}
	return nil
}

func (s *Service) GetReservation(ctx context.Context, id string) (*domain.Reservation, error) {
	return s.db().getReservation(ctx, id)
}

func (s *Service) ReservationHistory(ctx context.Context, id string) ([]domain.Transition, error) {
	return s.db().reservationHistory(ctx, id)
}

func (s *Service) CancelReservation(ctx context.Context, id, userID string) (*domain.Reservation, error) {
	err := s.inTx(ctx, func(st store) error {
		seat, r, err := s.lockReservationWithSeat(ctx, st, id)
		if err != nil {
			return err
		}
		if r.UserID != userID {
			return fmt.Errorf("%w: reservation %s belongs to another user", domain.ErrForbidden, id)
		}
		if r.Status != domain.ReservationReserved {
			return fmt.Errorf("%w: a %s reservation cannot be canceled", domain.ErrInvalidTransition, r.Status)
		}
		if r.Expired {
			if err := s.expireReservation(ctx, st, &seat, r, "reservation ttl (on cancel)"); err != nil {
				return err
			}
			return commitThen(fmt.Errorf("%w: reservation %s", domain.ErrReservationExpired, id))
		}
		if err := st.moveReservation(ctx, r.ID, r.Status, domain.ReservationCanceled, "canceled by user"); err != nil {
			return err
		}
		return st.moveSeat(ctx, &seat, domain.SeatAvailable, "reservation "+id+" canceled")
	})
	if err != nil {
		return nil, err
	}
	return s.GetReservation(ctx, id)
}

func (s *Service) ExpireReservation(ctx context.Context, id string) (bool, error) {
	expired := false
	err := s.inTx(ctx, func(st store) error {
		seat, r, err := s.lockReservationWithSeat(ctx, st, id)
		if err != nil || r.Status != domain.ReservationReserved || !r.Expired {
			return err
		}
		if err := s.expireReservation(ctx, st, &seat, r, "reservation ttl (sweeper)"); err != nil {
			return err
		}
		expired = true
		return nil
	})
	return expired, err
}

func (s *Service) ExpireDueReservations(ctx context.Context) (int, error) {
	return s.sweep(ctx,
		`SELECT id FROM reservations WHERE status = 'reserved' AND expires_at <= clock_timestamp() ORDER BY expires_at LIMIT $1`,
		s.ExpireReservation)
}

func (s *Service) lockReservationWithSeat(ctx context.Context, st store, id string) (seatRow, reservationRow, error) {
	eventID, number, err := st.reservationSeat(ctx, id)
	if err != nil {
		return seatRow{}, reservationRow{}, err
	}
	seat, err := st.lockSeat(ctx, eventID, number)
	if err != nil {
		return seatRow{}, reservationRow{}, err
	}
	r, err := st.lockReservation(ctx, id)
	return seat, r, err
}

func (s *Service) expireReservation(ctx context.Context, st store, seat *seatRow, r reservationRow, reason string) error {
	if err := st.moveReservation(ctx, r.ID, r.Status, domain.ReservationExpired, reason); err != nil {
		return err
	}
	return st.moveSeat(ctx, seat, domain.SeatAvailable, "reservation "+r.ID+" expired")
}

const sweepBatch = 500

func (s *Service) sweep(ctx context.Context, query string, one func(context.Context, string) (bool, error)) (int, error) {
	rows, err := s.pool.Query(ctx, query, sweepBatch)
	if err != nil {
		return 0, err
	}
	due, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, id := range due {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		done, err := s.sweepItem(ctx, id, one)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		if done {
			n++
		}
	}
	return n, errors.Join(errs...)
}

func (s *Service) sweepItem(ctx context.Context, id string, one func(context.Context, string) (bool, error)) (bool, error) {
	if s.cfg.SweepItemTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.SweepItemTimeout)
		defer cancel()
	}
	return one(ctx, id)
}
