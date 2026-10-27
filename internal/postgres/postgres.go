// Package postgres holds the database plumbing shared by the services: pool
// construction, transactions, migrations and error classification.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is implemented by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, rows pgx.CopyFromSource) (int64, error)
}

// Connect creates a connection pool and checks that the database is reachable.
// maxConns <= 0 keeps the pgxpool default.
func Connect(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		// pgx returns timestamptz in time.Local by default; the API speaks UTC.
		conn.TypeMap().RegisterType(&pgtype.Type{
			Name:  "timestamptz",
			OID:   pgtype.TimestamptzOID,
			Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
		})
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// InTx runs fn in a transaction and commits if fn returns nil. If fn fails,
// including because ctx was cancelled mid-way, everything is rolled back.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	// Neither the rollback nor the commit may be skipped because the client
	// went away: once fn has succeeded the work is complete, and aborting the
	// COMMIT halfway would only leave its outcome unknown.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(context.WithoutCancel(ctx))
}

const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeDuplicateDatabase   = "42P04"
)

func IsUniqueViolation(err error) bool     { return hasCode(err, codeUniqueViolation) }
func IsForeignKeyViolation(err error) bool { return hasCode(err, codeForeignKeyViolation) }
func IsDuplicateDatabase(err error) bool   { return hasCode(err, codeDuplicateDatabase) }

// IsTransient reports errors worth retrying later: no connection (SQLSTATE
// class 08), deadlock or serialization failure (40), too many clients (53),
// server shutting down (57P). If the connection broke during COMMIT the
// outcome is unknown, but every operation of the service is safe to repeat.
func IsTransient(err error) bool {
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		(strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "40") ||
			strings.HasPrefix(pgErr.Code, "53") || strings.HasPrefix(pgErr.Code, "57P"))
}

func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
