package booking

import (
	"context"
	"errors"
	"fmt"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/ids"
	"awesomeProject/internal/postgres"
)

const MaxSeatsPerEvent = 10_000

type NewOrganizer struct {
	ID      string
	Name    string
	Balance int64
}

func (s *Service) CreateOrganizer(ctx context.Context, in NewOrganizer) (*domain.Organizer, error) {
	if err := errors.Join(
		validateOptionalID("id", in.ID),
		validateName("name", in.Name),
		validateAmount("balance", in.Balance, 0),
	); err != nil {
		return nil, err
	}
	o := domain.Organizer{ID: in.ID, Name: in.Name, Balance: in.Balance}
	if o.ID == "" {
		o.ID = ids.New("org-")
	}
	err := s.db().insertOrganizer(ctx, o)
	if postgres.IsUniqueViolation(err) {
		return nil, fmt.Errorf("%w: organizer %s", domain.ErrAlreadyExists, o.ID)
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (s *Service) GetOrganizer(ctx context.Context, id string) (*domain.Organizer, error) {
	return s.db().getOrganizer(ctx, id)
}

type NewEvent struct {
	ID          string
	Name        string
	OrganizerID string
	Seats       []domain.SeatSpec
}

func (s *Service) CreateEvent(ctx context.Context, in NewEvent) (*domain.Event, error) {
	if err := errors.Join(
		validateOptionalID("id", in.ID),
		validateName("name", in.Name),
		requireField("organizerId", in.OrganizerID),
		validateSeats(in.Seats),
	); err != nil {
		return nil, err
	}
	e := domain.Event{ID: in.ID, Name: in.Name, OrganizerID: in.OrganizerID}
	if e.ID == "" {
		e.ID = ids.New("event-")
	}
	err := s.inTx(ctx, func(st store) error { return st.insertEvent(ctx, e, in.Seats) })
	switch {
	case postgres.IsUniqueViolation(err):
		return nil, fmt.Errorf("%w: event %s", domain.ErrAlreadyExists, e.ID)
	case postgres.IsForeignKeyViolation(err):
		return nil, fmt.Errorf("%w: organizer %s", domain.ErrNotFound, e.OrganizerID)
	case err != nil:
		return nil, err
	}
	return &e, nil
}

func validateSeats(seats []domain.SeatSpec) error {
	if len(seats) == 0 || len(seats) > MaxSeatsPerEvent {
		return validationErr("an event needs 1..%d seats", MaxSeatsPerEvent)
	}
	seen := make(map[string]bool, len(seats))
	for _, seat := range seats {
		if !idPattern.MatchString(seat.Number) {
			return validationErr("seat number %q must match %s", seat.Number, idPattern)
		}
		if seen[seat.Number] {
			return validationErr("seat %s is listed twice", seat.Number)
		}
		seen[seat.Number] = true
		if err := validateAmount("price of seat "+seat.Number, seat.Price, 1); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ListEvents(ctx context.Context) ([]domain.Event, error) {
	return s.db().listEvents(ctx)
}

func (s *Service) ListSeats(ctx context.Context, eventID string) ([]domain.Seat, error) {
	return s.db().listSeats(ctx, eventID)
}

func (s *Service) GetUser(ctx context.Context, id string) (*domain.User, error) {
	return s.db().getUser(ctx, id)
}
