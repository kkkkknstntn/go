package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockID is an arbitrary key for pg_advisory_xact_lock.
const migrationLockID = 7270001

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies the embedded migrations/NNNN_name.sql files that are not in
// schema_migrations yet. It runs in one transaction under an advisory lock, so
// instances starting at the same time do not race.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	return InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    int PRIMARY KEY,
			name       text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
		)`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			return err
		}
		versions, err := pgx.CollectRows(rows, pgx.RowTo[int])
		if err != nil {
			return err
		}
		applied := make(map[int]bool, len(versions))
		for _, v := range versions {
			applied[v] = true
		}
		if len(versions) == 0 {
			if err := refuseUnmanagedSchema(ctx, tx); err != nil {
				return err
			}
		}
		for _, m := range migrations {
			if applied[m.version] {
				continue
			}
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %s: %w", m.name, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
				return err
			}
		}
		return nil
	})
}

// refuseUnmanagedSchema fails when the tables exist but schema_migrations is
// empty, i.e. the schema was created without migrations: applying 0001 on top
// of it would fail with a less helpful "relation already exists".
func refuseUnmanagedSchema(ctx context.Context, tx pgx.Tx) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.seats') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errors.New("the database has tables but no schema_migrations: it was created by an older " +
			"version of the service; recreate it (docker compose down -v && docker compose up -d) or use an empty database")
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil {
			return nil, fmt.Errorf("migration %q: name must look like 0001_description.sql", e.Name())
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, e.Name(), version)
		}
		seen[version] = e.Name()
		body, err := fs.ReadFile(migrationFiles, "migrations/"+e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}
