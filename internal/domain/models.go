// Package domain holds the entities, their statuses and state machines, and the
// errors shared by all layers. It has no dependencies on storage or transport.
package domain

import "time"

// MaxAmount bounds prices and starting balances, keeping sums far from the
// int64 limit (PostgreSQL would reject an overflow anyway).
const MaxAmount int64 = 1_000_000_000_000

// User is an account: a buyer (RoleUser) or a staff account (RoleAdmin, RoleProvider).
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Login     string    `json:"login"`
	Role      Role      `json:"role"`
	Balance   int64     `json:"balance"`
	CreatedAt time.Time `json:"createdAt"`
}

type Organizer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Balance int64  `json:"balance"`
}

type Event struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OrganizerID string `json:"organizerId"`
}

// SeatSpec describes a seat when an event is created.
type SeatSpec struct {
	Number string `json:"number"`
	Price  int64  `json:"price"`
}

// Seat is a seat as shown to clients. A seat is identified by (event id, number).
type Seat struct {
	Number string     `json:"seat"`
	Price  int64      `json:"price"`
	Status SeatStatus `json:"status"`
}

type Reservation struct {
	ID         string            `json:"reservationId"`
	UserID     string            `json:"userId"`
	EventID    string            `json:"eventId"`
	SeatID     string            `json:"seatId"`
	Status     ReservationStatus `json:"status"`
	CreatedAt  time.Time         `json:"createdAt"`
	ExpiresAt  time.Time         `json:"expiresAt"`
	FinishedAt *time.Time        `json:"finishedAt,omitempty"`
}

type Purchase struct {
	ID            string         `json:"purchaseId"`
	ReservationID string         `json:"reservationId"`
	PaymentID     string         `json:"paymentId"`
	UserID        string         `json:"userId"`
	Status        PurchaseStatus `json:"status"`
	StartedAt     time.Time      `json:"startedAt"`
	FinishedAt    *time.Time     `json:"finishedAt,omitempty"`
}

type Payment struct {
	ID             string         `json:"paymentId"`
	PurchaseID     string         `json:"purchaseId"`
	ReservationID  string         `json:"reservationId"`
	UserID         string         `json:"userId"`
	OrganizerID    string         `json:"organizerId"`
	Amount         int64          `json:"amount"`
	Status         PaymentStatus  `json:"status"`
	FailureReason  string         `json:"failureReason,omitempty"`
	PurchaseStatus PurchaseStatus `json:"purchaseStatus"`
	CreatedAt      time.Time      `json:"createdAt"`
	ExpiresAt      time.Time      `json:"expiresAt"`
	CompletedAt    *time.Time     `json:"completedAt,omitempty"`
}

// PurchaseStarted is the result of starting a purchase.
type PurchaseStarted struct {
	PurchaseID       string         `json:"purchaseId"`
	PaymentID        string         `json:"paymentId"`
	Status           PurchaseStatus `json:"status"`
	Amount           int64          `json:"amount"`
	PaymentExpiresAt time.Time      `json:"paymentExpiresAt"`
}

// Transition is one entry of the state history.
type Transition struct {
	Entity   string    `json:"entity"`
	EntityID string    `json:"entityId"`
	From     string    `json:"from,omitempty"`
	To       string    `json:"to"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}
