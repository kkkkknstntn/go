package booking

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"awesomeProject/internal/domain"
)

func (s store) insertOrganizer(ctx context.Context, o domain.Organizer) error {
	_, err := s.q.Exec(ctx, `INSERT INTO organizers (id, name, balance) VALUES ($1, $2, $3)`, o.ID, o.Name, o.Balance)
	return err
}

func (s store) getOrganizer(ctx context.Context, id string) (*domain.Organizer, error) {
	var o domain.Organizer
	err := s.q.QueryRow(ctx, `SELECT id, name, balance FROM organizers WHERE id = $1`, id).Scan(&o.ID, &o.Name, &o.Balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: organizer %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (s store) insertEvent(ctx context.Context, e domain.Event, seats []domain.SeatSpec) error {
	if _, err := s.q.Exec(ctx, `INSERT INTO events (id, name, organizer_id) VALUES ($1, $2, $3)`,
		e.ID, e.Name, e.OrganizerID); err != nil {
		return err
	}
	rows := make([][]any, len(seats))
	for i, seat := range seats {
		rows[i] = []any{e.ID, seat.Number, i, seat.Price}
	}
	_, err := s.q.CopyFrom(ctx, pgx.Identifier{"seats"},
		[]string{"event_id", "number", "seat_order", "price"}, pgx.CopyFromRows(rows))
	return err
}

func (s store) eventOrganizer(ctx context.Context, eventID string) (string, error) {
	var id string
	err := s.q.QueryRow(ctx, `SELECT organizer_id FROM events WHERE id = $1`, eventID).Scan(&id)
	return id, err
}

func (s store) listEvents(ctx context.Context) ([]domain.Event, error) {
	rows, err := s.q.Query(ctx, `SELECT id, name, organizer_id FROM events ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Event, error) {
		var e domain.Event
		err := row.Scan(&e.ID, &e.Name, &e.OrganizerID)
		return e, err
	})
}

func (s store) getUser(ctx context.Context, id string) (*domain.User, error) {
	var u domain.User
	err := s.q.QueryRow(ctx, `SELECT id, name, login, role, balance, created_at FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Name, &u.Login, &u.Role, &u.Balance, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: user %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s store) userRole(ctx context.Context, id string) (domain.Role, error) {
	var role domain.Role
	err := s.q.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, id).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: user %s", domain.ErrNotFound, id)
	}
	return role, err
}
