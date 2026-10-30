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

const MaxConns = 15

var (
	once    sync.Once
	pool    *pgxpool.Pool
	initErr error
)

func URL() string {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = defaultURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		return base
	}
	u.Path = "/" + strings.TrimPrefix(u.Path, "/") + "_" + packageName()
	return u.String()
}

var notIdent = regexp.MustCompile(`[^a-z0-9_]+`)

func packageName() string {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")
	return notIdent.ReplaceAllString(strings.ToLower(name), "_")
}

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
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var exists bool
	if err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
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
