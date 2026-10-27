package domain

import "testing"

func TestReservationStateMachine(t *testing.T) {
	allowed := map[[2]ReservationStatus]bool{
		{ReservationReserved, ReservationPaymentPending}:      true,
		{ReservationReserved, ReservationCanceled}:            true,
		{ReservationReserved, ReservationExpired}:             true,
		{ReservationPaymentPending, ReservationPurchased}:     true,
		{ReservationPaymentPending, ReservationPaymentFailed}: true,
		{ReservationPaymentPending, ReservationExpired}:       true,
	}
	all := []ReservationStatus{ReservationReserved, ReservationPaymentPending, ReservationPurchased,
		ReservationCanceled, ReservationExpired, ReservationPaymentFailed}
	for _, from := range all {
		for _, to := range all {
			if got := from.CanTransitionTo(to); got != allowed[[2]ReservationStatus{from, to}] {
				t.Errorf("%s -> %s: got %v", from, to, got)
			}
		}
	}
	for _, s := range []ReservationStatus{ReservationPurchased, ReservationCanceled, ReservationExpired, ReservationPaymentFailed} {
		if !s.IsFinal() {
			t.Errorf("%s must be final", s)
		}
	}
	// the example from ТЗ §35
	if ReservationPurchased.CanTransitionTo(ReservationCanceled) {
		t.Error("purchased -> canceled must be forbidden")
	}
}

func TestPaymentAndPurchaseFinalStates(t *testing.T) {
	for _, s := range []PaymentStatus{PaymentSucceeded, PaymentFailed, PaymentTimedOut} {
		if !s.IsFinal() || s.CanTransitionTo(PaymentPending) || s.CanTransitionTo(PaymentSucceeded) {
			t.Errorf("payment %s must be final", s)
		}
	}
	if PaymentPending.IsFinal() {
		t.Error("pending is not final")
	}
	if PaymentTimedOut.CanTransitionTo(PaymentSucceeded) {
		t.Error("timed_out -> succeeded must be forbidden")
	}
	for _, s := range []PurchaseStatus{PurchasePurchased, PurchaseFailed, PurchaseTimedOut} {
		if !s.IsFinal() {
			t.Errorf("purchase %s must be final", s)
		}
	}
	if PurchaseTimedOut.CanTransitionTo(PurchasePurchased) {
		t.Error("timed_out -> purchased must be forbidden")
	}
}

func TestSeatStateMachine(t *testing.T) {
	cases := []struct {
		from, to SeatStatus
		ok       bool
	}{
		{SeatAvailable, SeatReserved, true},
		{SeatAvailable, SeatPurchased, false},
		{SeatReserved, SeatPaymentPending, true},
		{SeatReserved, SeatAvailable, true},
		{SeatPaymentPending, SeatPurchased, true},
		{SeatPaymentPending, SeatAvailable, true},
		{SeatPurchased, SeatAvailable, false},
		{SeatPurchased, SeatReserved, false},
	}
	for _, c := range cases {
		if got := c.from.CanTransitionTo(c.to); got != c.ok {
			t.Errorf("%s -> %s: got %v, want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleUser, RoleAdmin, RoleProvider} {
		if !r.Valid() {
			t.Errorf("%s must be valid", r)
		}
	}
	if Role("root").Valid() || Role("").Valid() {
		t.Error("unknown roles must be invalid")
	}
}
