package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReservationTTL != 5*time.Minute || cfg.PaymentTimeout != 10*time.Minute ||
		cfg.WorkerInterval != 5*time.Second || cfg.ListenAddr != ":8080" || !cfg.Seed ||
		cfg.LogLevel != slog.LevelInfo || cfg.DatabaseURL != DefaultDatabaseURL {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if !cfg.Auth.JWTSecretGenerated || len(cfg.Auth.JWTSecret) < 32 {
		t.Fatal("a random JWT secret must be generated when JWT_SECRET is unset")
	}
	if cfg.Admin.Login != "admin" || !cfg.Admin.DefaultPassword || cfg.Admin.Password == "" {
		t.Fatalf("seed mode must provide a development admin: %+v", cfg.Admin)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"RESERVATION_TTL": "5s",
		"PAYMENT_TIMEOUT": "10s",
		"JWT_SECRET":      strings.Repeat("s", 40),
		"SEED":            "false",
		"ADMIN_PASSWORD":  "real-admin-password",
		"LOG_LEVEL":       "debug",
		"DB_MAX_CONNS":    "25",
		"REQUEST_TIMEOUT": " 3s ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReservationTTL != 5*time.Second || cfg.PaymentTimeout != 10*time.Second || cfg.DBMaxConns != 25 ||
		cfg.RequestTimeout != 3*time.Second ||
		cfg.LogLevel != slog.LevelDebug || cfg.Auth.JWTSecretGenerated || cfg.Seed {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.Admin.Password != "real-admin-password" || cfg.Admin.DefaultPassword {
		t.Fatalf("admin: %+v", cfg.Admin)
	}
	if cfg.Provider.Password != "" {
		t.Fatal("outside seed mode there is no default provider password")
	}
}

func TestInvalidValuesAreAllReported(t *testing.T) {
	_, err := Load(env(map[string]string{
		"RESERVATION_TTL": "five minutes",
		"PAYMENT_TIMEOUT": "-1s",
		"JWT_SECRET":      "too-short",
		"SEED":            "maybe",
		"BCRYPT_COST":     "99",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"RESERVATION_TTL", "PAYMENT_TIMEOUT", "JWT_SECRET", "SEED", "BCRYPT_COST"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}
