package domain

import "slices"

// Allowed status transitions. The booking store checks every status change
// against these tables before running the UPDATE.

// SeatStatus is the stored state of a seat.
type SeatStatus string

const (
	SeatAvailable      SeatStatus = "available"
	SeatReserved       SeatStatus = "reserved"
	SeatPaymentPending SeatStatus = "payment_pending"
	SeatPurchased      SeatStatus = "purchased"
)

var seatTransitions = map[SeatStatus][]SeatStatus{
	SeatAvailable:      {SeatReserved},
	SeatReserved:       {SeatAvailable, SeatPaymentPending},
	SeatPaymentPending: {SeatAvailable, SeatPurchased},
}

func (s SeatStatus) CanTransitionTo(to SeatStatus) bool {
	return slices.Contains(seatTransitions[s], to)
}

// ReservationStatus is the state of a reservation.
type ReservationStatus string

const (
	ReservationReserved       ReservationStatus = "reserved"
	ReservationPaymentPending ReservationStatus = "payment_pending"
	ReservationPurchased      ReservationStatus = "purchased"
	ReservationCanceled       ReservationStatus = "canceled"
	ReservationExpired        ReservationStatus = "expired"
	ReservationPaymentFailed  ReservationStatus = "payment_failed"
)

var reservationTransitions = map[ReservationStatus][]ReservationStatus{
	ReservationReserved:       {ReservationPaymentPending, ReservationCanceled, ReservationExpired},
	ReservationPaymentPending: {ReservationPurchased, ReservationPaymentFailed, ReservationExpired},
}

func (s ReservationStatus) CanTransitionTo(to ReservationStatus) bool {
	return slices.Contains(reservationTransitions[s], to)
}

// IsFinal reports whether no transition leaves s.
func (s ReservationStatus) IsFinal() bool { return len(reservationTransitions[s]) == 0 }

// PurchaseStatus is the state of a purchase.
type PurchaseStatus string

const (
	PurchasePaymentPending PurchaseStatus = "payment_pending"
	PurchasePurchased      PurchaseStatus = "purchased"
	PurchaseFailed         PurchaseStatus = "failed"
	PurchaseTimedOut       PurchaseStatus = "timed_out"
)

var purchaseTransitions = map[PurchaseStatus][]PurchaseStatus{
	PurchasePaymentPending: {PurchasePurchased, PurchaseFailed, PurchaseTimedOut},
}

func (s PurchaseStatus) CanTransitionTo(to PurchaseStatus) bool {
	return slices.Contains(purchaseTransitions[s], to)
}

func (s PurchaseStatus) IsFinal() bool { return len(purchaseTransitions[s]) == 0 }

// PaymentStatus is the state of a payment attempt.
type PaymentStatus string

const (
	PaymentPending   PaymentStatus = "pending"
	PaymentSucceeded PaymentStatus = "succeeded"
	PaymentFailed    PaymentStatus = "failed"
	PaymentTimedOut  PaymentStatus = "timed_out"
)

var paymentTransitions = map[PaymentStatus][]PaymentStatus{
	PaymentPending: {PaymentSucceeded, PaymentFailed, PaymentTimedOut},
}

func (s PaymentStatus) CanTransitionTo(to PaymentStatus) bool {
	return slices.Contains(paymentTransitions[s], to)
}

func (s PaymentStatus) IsFinal() bool { return len(paymentTransitions[s]) == 0 }

// Failure reasons stored on failed payments.
const (
	FailureInsufficientFunds = "insufficient_funds"
	FailureDeclined          = "declined"
)

// Role is an account role carried in access tokens.
type Role string

const (
	// RoleUser is a buyer with a test balance.
	RoleUser Role = "user"
	// RoleAdmin manages organizers and events and may read everything.
	RoleAdmin Role = "admin"
	// RoleProvider models the external payment system that sends confirm/fail callbacks.
	RoleProvider Role = "provider"
)

func (r Role) Valid() bool {
	switch r {
	case RoleUser, RoleAdmin, RoleProvider:
		return true
	}
	return false
}
