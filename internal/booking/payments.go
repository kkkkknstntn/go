package booking

import (
	"context"
	"errors"
	"fmt"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/ids"
)

func (s *Service) StartPurchase(ctx context.Context, reservationID, userID string) (*domain.PurchaseStarted, error) {
	var out *domain.PurchaseStarted
	err := s.inTx(ctx, func(st store) error {
		seat, r, err := s.lockReservationWithSeat(ctx, st, reservationID)
		if err != nil {
			return err
		}
		if r.UserID != userID {
			return fmt.Errorf("%w: reservation %s belongs to another user", domain.ErrForbidden, reservationID)
		}
		if r.Status != domain.ReservationReserved {
			return fmt.Errorf("%w: cannot start a purchase for a %s reservation", domain.ErrInvalidTransition, r.Status)
		}
		if r.Expired {
			if err = s.expireReservation(ctx, st, &seat, r, "reservation ttl (on purchase)"); err != nil {
				return err
			}
			return commitThen(fmt.Errorf("%w: reservation %s", domain.ErrReservationExpired, reservationID))
		}
		organizerID, err := st.eventOrganizer(ctx, seat.EventID)
		if err != nil {
			return err
		}
		purchaseID, paymentID := ids.New("purchase-"), ids.New("payment-")
		if err = st.insertPurchase(ctx, purchaseID, reservationID); err != nil {
			return err
		}
		expiresAt, err := st.insertPayment(ctx, paymentID, purchaseID, userID, organizerID, seat.Price, s.cfg.PaymentTimeout)
		if err != nil {
			return err
		}
		if err := st.moveReservation(ctx, r.ID, r.Status, domain.ReservationPaymentPending, "purchase "+purchaseID+" started"); err != nil {
			return err
		}
		if err := st.moveSeat(ctx, &seat, domain.SeatPaymentPending, "purchase "+purchaseID+" started"); err != nil {
			return err
		}
		out = &domain.PurchaseStarted{
			PurchaseID:       purchaseID,
			PaymentID:        paymentID,
			Status:           domain.PurchasePaymentPending,
			Amount:           seat.Price,
			PaymentExpiresAt: expiresAt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) GetPurchase(ctx context.Context, id string) (*domain.Purchase, error) {
	return s.db().getPurchase(ctx, id)
}

func (s *Service) GetPayment(ctx context.Context, id string) (*domain.Payment, error) {
	return s.db().getPayment(ctx, id)
}

func (s *Service) ConfirmPayment(ctx context.Context, paymentID string) (*domain.Payment, error) {
	err := s.inTx(ctx, func(st store) error {
		seat, p, err := s.lockPaymentWithSeat(ctx, st, paymentID)
		if err != nil {
			return err
		}
		switch p.Status {
		case domain.PaymentSucceeded:
			return nil
		case domain.PaymentTimedOut:
			return fmt.Errorf("%w: payment %s, confirmation arrived too late", domain.ErrPaymentTimedOut, paymentID)
		case domain.PaymentFailed:
			return fmt.Errorf("%w: payment %s has already failed", domain.ErrPaymentNotPending, paymentID)
		}
		if p.Expired {
			if err = s.closePayment(ctx, st, &seat, &p, outcomeTimedOut, "payment timeout (on confirm)"); err != nil {
				return err
			}
			return commitThen(fmt.Errorf("%w: payment %s, confirmation arrived too late", domain.ErrPaymentTimedOut, paymentID))
		}
		balance, err := st.lockBalance(ctx, p.UserID)
		if err != nil {
			return err
		}
		if balance < p.Amount {
			if err := s.closePayment(ctx, st, &seat, &p, outcomeFailed, domain.FailureInsufficientFunds); err != nil {
				return err
			}
			return commitThen(fmt.Errorf("%w: balance %d, price %d", domain.ErrInsufficientFunds, balance, p.Amount))
		}
		if err := st.transfer(ctx, p.UserID, p.OrganizerID, p.Amount); err != nil {
			return err
		}
		return s.closePayment(ctx, st, &seat, &p, outcomeSucceeded, "confirmed by provider")
	})
	return s.paymentResult(ctx, paymentID, err)
}

func (s *Service) FailPayment(ctx context.Context, paymentID string) (*domain.Payment, error) {
	err := s.inTx(ctx, func(st store) error {
		seat, p, err := s.lockPaymentWithSeat(ctx, st, paymentID)
		if err != nil {
			return err
		}
		switch p.Status {
		case domain.PaymentFailed:
			return nil
		case domain.PaymentSucceeded:
			return fmt.Errorf("%w: payment %s has already succeeded", domain.ErrPaymentNotPending, paymentID)
		case domain.PaymentTimedOut:
			return fmt.Errorf("%w: payment %s", domain.ErrPaymentTimedOut, paymentID)
		}
		if p.Expired {
			if err := s.closePayment(ctx, st, &seat, &p, outcomeTimedOut, "payment timeout (on fail)"); err != nil {
				return err
			}
			return commitThen(fmt.Errorf("%w: payment %s", domain.ErrPaymentTimedOut, paymentID))
		}
		return s.closePayment(ctx, st, &seat, &p, outcomeFailed, domain.FailureDeclined)
	})
	return s.paymentResult(ctx, paymentID, err)
}

func (s *Service) TimeoutPayment(ctx context.Context, paymentID string) (bool, error) {
	timedOut := false
	err := s.inTx(ctx, func(st store) error {
		seat, p, err := s.lockPaymentWithSeat(ctx, st, paymentID)
		if err != nil || p.Status != domain.PaymentPending || !p.Expired {
			return err
		}
		if err := s.closePayment(ctx, st, &seat, &p, outcomeTimedOut, "payment timeout (sweeper)"); err != nil {
			return err
		}
		timedOut = true
		return nil
	})
	return timedOut, err
}

func (s *Service) TimeoutDuePayments(ctx context.Context) (int, error) {
	return s.sweep(ctx,
		`SELECT id FROM payments WHERE status = 'pending' AND expires_at <= clock_timestamp() ORDER BY expires_at LIMIT $1`,
		s.TimeoutPayment)
}

func (s *Service) lockPaymentWithSeat(ctx context.Context, st store, paymentID string) (seatRow, paymentRow, error) {
	eventID, number, err := st.paymentSeat(ctx, paymentID)
	if err != nil {
		return seatRow{}, paymentRow{}, err
	}
	seat, err := st.lockSeat(ctx, eventID, number)
	if err != nil {
		return seatRow{}, paymentRow{}, err
	}
	p, err := st.lockPayment(ctx, paymentID)
	return seat, p, err
}

type paymentOutcome struct {
	payment     domain.PaymentStatus
	purchase    domain.PurchaseStatus
	reservation domain.ReservationStatus
	seat        domain.SeatStatus
}

var (
	outcomeSucceeded = paymentOutcome{domain.PaymentSucceeded, domain.PurchasePurchased, domain.ReservationPurchased, domain.SeatPurchased}
	outcomeFailed    = paymentOutcome{domain.PaymentFailed, domain.PurchaseFailed, domain.ReservationPaymentFailed, domain.SeatAvailable}
	outcomeTimedOut  = paymentOutcome{domain.PaymentTimedOut, domain.PurchaseTimedOut, domain.ReservationExpired, domain.SeatAvailable}
)

func (s *Service) closePayment(ctx context.Context, st store, seat *seatRow, p *paymentRow, o paymentOutcome, reason string) error {
	if err := st.movePayment(ctx, p, o.payment, reason); err != nil {
		return err
	}
	if err := st.movePurchase(ctx, p.PurchaseID, domain.PurchasePaymentPending, o.purchase, reason); err != nil {
		return err
	}
	if err := st.moveReservation(ctx, p.ReservationID, domain.ReservationPaymentPending, o.reservation, reason); err != nil {
		return err
	}
	return st.moveSeat(ctx, seat, o.seat, "payment "+p.ID+" "+string(o.payment))
}

func (s *Service) paymentResult(ctx context.Context, paymentID string, opErr error) (*domain.Payment, error) {
	if opErr != nil && !isPaymentRejection(opErr) {
		return nil, opErr
	}
	p, err := s.GetPayment(ctx, paymentID)
	if err != nil {
		return nil, err
	}
	return p, opErr
}

func isPaymentRejection(err error) bool {
	return errors.Is(err, domain.ErrInsufficientFunds) ||
		errors.Is(err, domain.ErrPaymentTimedOut) ||
		errors.Is(err, domain.ErrPaymentNotPending)
}
