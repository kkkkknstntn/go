package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"awesomeProject/internal/auth"
	"awesomeProject/internal/booking"
	"awesomeProject/internal/config"
	"awesomeProject/internal/domain"
)

const (
	demoOrganizerID = "concert-company"
	demoEventID     = "event-1"
)

func ensureStaffAccounts(ctx context.Context, cfg config.Config, log *slog.Logger, svc *auth.Service) error {
	for _, acc := range []struct {
		cfg         config.Account
		role        domain.Role
		name        string
		passwordEnv string
	}{
		{cfg.Admin, domain.RoleAdmin, "Administrator", "ADMIN_PASSWORD"},
		{cfg.Provider, domain.RoleProvider, "Payment provider", "PROVIDER_PASSWORD"},
	} {
		if acc.cfg.Password == "" {
			continue
		}
		created, err := svc.EnsureAccount(ctx, acc.name, acc.cfg.Login, acc.cfg.Password, acc.role)
		if err != nil {
			return fmt.Errorf("create %s account: %w", acc.role, err)
		}
		if created {
			log.Info("staff account created", "role", acc.role, "login", acc.cfg.Login)
		}
		if acc.cfg.DefaultPassword {
			log.Warn("staff account uses the documented development password; set "+acc.passwordEnv+" outside development",
				"login", acc.cfg.Login)
		}
	}
	return nil
}

func seedCatalog(ctx context.Context, svc *booking.Service) error {
	_, err := svc.CreateOrganizer(ctx, booking.NewOrganizer{ID: demoOrganizerID, Name: "Concert Company", Balance: 50_000})
	if err != nil && !errors.Is(err, domain.ErrAlreadyExists) {
		return err
	}
	seats := make([]domain.SeatSpec, 0, 100)
	for i := 1; i <= 100; i++ {
		price := int64(2000)
		if i > 80 {
			price = 2500
		}
		seats = append(seats, domain.SeatSpec{Number: fmt.Sprintf("A%d", i), Price: price})
	}
	_, err = svc.CreateEvent(ctx, booking.NewEvent{ID: demoEventID, Name: "Concert", OrganizerID: demoOrganizerID, Seats: seats})
	if err != nil && !errors.Is(err, domain.ErrAlreadyExists) {
		return err
	}
	return nil
}
