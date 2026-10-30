package booking

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/postgres"
)

type Config struct {
	ReservationTTL   time.Duration
	PaymentTimeout   time.Duration
	SweepItemTimeout time.Duration
}

type Service struct {
	pool *pgxpool.Pool
	cfg  Config
}

func New(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if cfg.ReservationTTL <= 0 || cfg.PaymentTimeout <= 0 {
		return nil, errors.New("booking: reservation TTL and payment timeout must be positive")
	}
	return &Service{pool: pool, cfg: cfg}, nil
}

func (s *Service) db() store { return store{q: s.pool} }

func (s *Service) inTx(ctx context.Context, fn func(st store) error) error {
	var kept error
	err := postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		err := fn(store{q: tx})
		var c committed
		if errors.As(err, &c) {
			kept = c.err
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	return kept
}

type committed struct{ err error }

func (c committed) Error() string { return c.err.Error() }
func (c committed) Unwrap() error { return c.err }

func commitThen(err error) error { return committed{err: err} }

var idPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validationErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrValidation, fmt.Sprintf(format, args...))
}

func validateName(field, v string) error {
	if strings.TrimSpace(v) == "" || utf8.RuneCountInString(v) > 200 {
		return validationErr("%s must be 1..200 characters", field)
	}
	return nil
}

func validateAmount(field string, v, minimum int64) error {
	if v < minimum || v > domain.MaxAmount {
		return validationErr("%s must be in %d..%d", field, minimum, domain.MaxAmount)
	}
	return nil
}

func validateOptionalID(field, v string) error {
	if v != "" && !idPattern.MatchString(v) {
		return validationErr("%s must match %s", field, idPattern)
	}
	return nil
}

func requireField(field, v string) error {
	if v == "" {
		return validationErr("%s is required", field)
	}
	return nil
}
