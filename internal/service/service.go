// Package service implements seat reservation and purchase on top of PostgreSQL.
//
// Concurrency model: every state transition of a seat (reserve, cancel, expire,
// start purchase, confirm, fail, timeout) starts with SELECT ... FOR UPDATE on the
// seat row, so all operations on one seat are serialized by the database, no matter
// which application instance issued them. Money rows are locked afterwards, always
// in the order user -> organizer, which rules out deadlocks.
package service

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrNotFound           = errors.New("not found")
	ErrAlreadyExists      = errors.New("already exists")
	ErrBadRequest         = errors.New("bad request")
	ErrForbidden          = errors.New("reservation belongs to another user")
	ErrSeatUnavailable    = errors.New("seat is not available")
	ErrInvalidState       = errors.New("invalid state transition")
	ErrReservationExpired = errors.New("reservation expired")
	ErrPaymentTimedOut    = errors.New("payment timed out")
	ErrPaymentNotPending  = errors.New("payment is not pending")
	ErrInsufficientFunds  = errors.New("insufficient funds")
)

const (
	SeatAvailable      = "available"
	SeatReserved       = "reserved"
	SeatPaymentPending = "payment_pending"
	SeatPurchased      = "purchased"

	ResReserved       = "reserved"
	ResPaymentPending = "payment_pending"
	ResPurchased      = "purchased"
	ResCanceled       = "canceled"
	ResExpired        = "expired"
	ResPaymentFailed  = "payment_failed"

	PurchasePending  = "payment_pending"
	PurchasePurchase = "purchased"
	PurchaseFailed   = "failed"
	PurchaseTimedOut = "timed_out"

	PayPending   = "pending"
	PaySucceeded = "succeeded"
	PayFailed    = "failed"
	PayTimedOut  = "timed_out"
)

type Config struct {
	ReservationTTL time.Duration
	PaymentTimeout time.Duration
}

type Service struct {
	pool *pgxpool.Pool
	cfg  Config
}

func New(pool *pgxpool.Pool, cfg Config) *Service {
	return &Service{pool: pool, cfg: cfg}
}

// Migrate applies the schema. The advisory lock makes it safe to start
// several instances at once.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7270001)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return tx.Commit(ctx)
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// inTx runs fn in a transaction. Any error (including context cancellation)
// rolls everything back, so a cancelled request never leaves partial state.
func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isFK(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func logTransition(ctx context.Context, q querier, entity, id, from, to, reason string) error {
	_, err := q.Exec(ctx,
		`INSERT INTO transitions (entity, entity_id, from_status, to_status, reason) VALUES ($1,$2,$3,$4,$5)`,
		entity, id, from, to, reason)
	return err
}
