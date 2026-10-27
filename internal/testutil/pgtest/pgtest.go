// Package pgtest gives integration tests a migrated PostgreSQL database.
//
// Each test package gets its own database: the one from TEST_DATABASE_URL
// (default tickets_test on localhost) with the package name appended, such as
// tickets_test_booking. go test runs packages in parallel, and with a shared
// database the sweeper started by the app tests would expire the booking
// tests' fixtures. A keyword/value DSN is used as is, without the suffix.
//
// Tests are skipped when PostgreSQL is unreachable, unless REQUIRE_DB is set.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"awesomeProject/internal/postgres"
)

const defaultURL = "postgres://postgres:postgres@localhost:5432/tickets_test?sslmode=disable"

// MaxConns keeps all test packages together under PostgreSQL's default
// max_connections of 100.
const MaxConns = 15

var (
	once    sync.Once
	pool    *pgxpool.Pool
	initErr error
)

// URL returns the database URL of the current test package.
func URL() string {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = defaultURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		return base // not a URL (keyword=value DSN): use it as is
	}
	u.Path = "/" + strings.TrimPrefix(u.Path, "/") + "_" + packageName()
	return u.String()
}

var notIdent = regexp.MustCompile(`[^a-z0-9_]+`)

// packageName derives the package from the test binary name ("booking.test").
func packageName() string {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")
	return notIdent.ReplaceAllString(strings.ToLower(name), "_")
}

// Pool returns a shared pool to the migrated test database, or skips the test.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	once.Do(func() { pool, initErr = setup(URL()) })
	if initErr != nil {
		if os.Getenv("REQUIRE_DB") != "" {
			t.Fatalf("test database unavailable: %v", initErr)
		}
		t.Skipf("PostgreSQL unavailable (%v); set TEST_DATABASE_URL, or REQUIRE_DB=1 to fail instead of skipping", initErr)
	}
	return pool
}

func setup(url string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ensureDatabase(ctx, url); err != nil {
		return nil, err
	}
	p, err := postgres.Connect(ctx, url, MaxConns)
	if err != nil {
		return nil, err
	}
	if err := postgres.Migrate(ctx, p); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// ensureDatabase creates the database if needed. Another test binary may be
// creating it at the same moment, so "already exists" is not an error.
func ensureDatabase(ctx context.Context, url string) error {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return err
	}
	name := cfg.Database
	cfg.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	if err != nil && !postgres.IsDuplicateDatabase(err) && !postgres.IsUniqueViolation(err) {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}
