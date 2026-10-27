package service

import "time"

type User struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Balance int64  `json:"balance"`
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

type SeatSpec struct {
	Number string `json:"number"`
	Price  int64  `json:"price"`
}

type Seat struct {
	Seat   string `json:"seat"`
	Price  int64  `json:"price"`
	Status string `json:"status"`
}

type Reservation struct {
	ID         string     `json:"reservationId"`
	UserID     string     `json:"userId"`
	EventID    string     `json:"eventId"`
	SeatID     string     `json:"seatId"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type Purchase struct {
	ID            string     `json:"purchaseId"`
	ReservationID string     `json:"reservationId"`
	PaymentID     string     `json:"paymentId"`
	Status        string     `json:"status"`
	StartedAt     time.Time  `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
}

type Payment struct {
	ID             string     `json:"paymentId"`
	PurchaseID     string     `json:"purchaseId"`
	UserID         string     `json:"userId"`
	OrganizerID    string     `json:"organizerId"`
	Amount         int64      `json:"amount"`
	Status         string     `json:"status"`
	FailureReason  string     `json:"failureReason,omitempty"`
	PurchaseStatus string     `json:"purchaseStatus"`
	CreatedAt      time.Time  `json:"createdAt"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
}

type PurchaseStarted struct {
	PurchaseID       string    `json:"purchaseId"`
	PaymentID        string    `json:"paymentId"`
	Status           string    `json:"status"`
	Amount           int64     `json:"amount"`
	PaymentExpiresAt time.Time `json:"paymentExpiresAt"`
}

type Transition struct {
	Entity   string    `json:"entity"`
	EntityID string    `json:"entityId"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}
