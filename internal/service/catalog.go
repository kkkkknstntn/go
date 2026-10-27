package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Service) CreateUser(ctx context.Context, id, name string, balance int64) (*User, error) {
	if strings.TrimSpace(name) == "" || balance < 0 {
		return nil, fmt.Errorf("%w: name is required and balance must be >= 0", ErrBadRequest)
	}
	if id == "" {
		id = newID("user-")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO users (id, name, balance) VALUES ($1,$2,$3)`, id, name, balance)
	if isUnique(err) {
		return nil, fmt.Errorf("%w: user %s", ErrAlreadyExists, id)
	}
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Name: name, Balance: balance}, nil
}

func (s *Service) GetUser(ctx context.Context, id string) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `SELECT id, name, balance FROM users WHERE id=$1`, id).Scan(&u.ID, &u.Name, &u.Balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: user %s", ErrNotFound, id)
	}
	return &u, err
}

func (s *Service) CreateOrganizer(ctx context.Context, id, name string, balance int64) (*Organizer, error) {
	if strings.TrimSpace(name) == "" || balance < 0 {
		return nil, fmt.Errorf("%w: name is required and balance must be >= 0", ErrBadRequest)
	}
	if id == "" {
		id = newID("org-")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO organizers (id, name, balance) VALUES ($1,$2,$3)`, id, name, balance)
	if isUnique(err) {
		return nil, fmt.Errorf("%w: organizer %s", ErrAlreadyExists, id)
	}
	if err != nil {
		return nil, err
	}
	return &Organizer{ID: id, Name: name, Balance: balance}, nil
}

func (s *Service) GetOrganizer(ctx context.Context, id string) (*Organizer, error) {
	var o Organizer
	err := s.pool.QueryRow(ctx, `SELECT id, name, balance FROM organizers WHERE id=$1`, id).Scan(&o.ID, &o.Name, &o.Balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: organizer %s", ErrNotFound, id)
	}
	return &o, err
}

// CreateEvent creates an event together with its seats in one transaction.
func (s *Service) CreateEvent(ctx context.Context, id, name, organizerID string, seats []SeatSpec) (*Event, error) {
	if strings.TrimSpace(name) == "" || len(seats) == 0 {
		return nil, fmt.Errorf("%w: name and at least one seat are required", ErrBadRequest)
	}
	seen := map[string]bool{}
	for _, st := range seats {
		if st.Number == "" || st.Price <= 0 || seen[st.Number] {
			return nil, fmt.Errorf("%w: seat %q must have a unique number and a positive price", ErrBadRequest, st.Number)
		}
		seen[st.Number] = true
	}
	if id == "" {
		id = newID("event-")
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO events (id, name, organizer_id) VALUES ($1,$2,$3)`, id, name, organizerID); err != nil {
			return err
		}
		b := &pgx.Batch{}
		for i, st := range seats {
			b.Queue(`INSERT INTO seats (event_id, number, ord, price) VALUES ($1,$2,$3,$4)`, id, st.Number, i, st.Price)
		}
		br := tx.SendBatch(ctx, b)
		for range seats {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return err
			}
		}
		return br.Close()
	})
	switch {
	case isUnique(err):
		return nil, fmt.Errorf("%w: event %s", ErrAlreadyExists, id)
	case isFK(err):
		return nil, fmt.Errorf("%w: organizer %s", ErrNotFound, organizerID)
	case err != nil:
		return nil, err
	}
	return &Event{ID: id, Name: name, OrganizerID: organizerID}, nil
}

func (s *Service) ListEvents(ctx context.Context) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, organizer_id FROM events ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Name, &e.OrganizerID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListSeats reports the effective status: a seat whose reservation has already
// expired is shown as available even if the worker has not processed it yet.
func (s *Service) ListSeats(ctx context.Context, eventID string) ([]Seat, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id=$1)`, eventID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: event %s", ErrNotFound, eventID)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT s.number, s.price,
		       CASE WHEN s.status = 'reserved' AND r.expires_at <= clock_timestamp()
		            THEN 'available' ELSE s.status END
		FROM seats s
		LEFT JOIN reservations r
		       ON r.event_id = s.event_id AND r.seat_number = s.number AND r.status = 'reserved'
		WHERE s.event_id = $1
		ORDER BY s.ord`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Seat{}
	for rows.Next() {
		var st Seat
		if err := rows.Scan(&st.Seat, &st.Price, &st.Status); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Service) Transitions(ctx context.Context, entityID string) ([]Transition, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT entity, entity_id, from_status, to_status, reason, at FROM transitions WHERE entity_id=$1 ORDER BY id`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Transition{}
	for rows.Next() {
		var t Transition
		if err := rows.Scan(&t.Entity, &t.EntityID, &t.From, &t.To, &t.Reason, &t.At); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
