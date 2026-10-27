package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestErrorClassification(t *testing.T) {
	pg := func(code string) error { return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code}) }
	cases := []struct {
		err                        error
		unique, foreign, transient bool
	}{
		{pg("23505"), true, false, false},
		{pg("23503"), false, true, false},
		{pg("40P01"), false, false, true}, // deadlock_detected
		{pg("40001"), false, false, true}, // serialization_failure
		{pg("53300"), false, false, true}, // too_many_connections
		{pg("57P01"), false, false, true}, // admin_shutdown
		{pg("08006"), false, false, true}, // connection_failure
		{pg("22001"), false, false, false},
		{errors.New("plain"), false, false, false},
	}
	for _, c := range cases {
		if IsUniqueViolation(c.err) != c.unique || IsForeignKeyViolation(c.err) != c.foreign || IsTransient(c.err) != c.transient {
			t.Errorf("%v: unique=%v foreign=%v transient=%v", c.err, IsUniqueViolation(c.err), IsForeignKeyViolation(c.err), IsTransient(c.err))
		}
	}
}

func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || ms[0].version != 1 {
		t.Fatalf("migrations: %+v", ms)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].version <= ms[i-1].version {
			t.Fatalf("migrations are not ordered: %s after %s", ms[i].name, ms[i-1].name)
		}
	}
}
