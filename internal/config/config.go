// Package config reads the application configuration from environment variables.
package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const DefaultDatabaseURL = "postgres://postgres:postgres@localhost:5432/tickets?sslmode=disable"

type Config struct {
	DatabaseURL string
	DBMaxConns  int32 // 0: pgxpool default
	ListenAddr  string

	ReservationTTL  time.Duration
	PaymentTimeout  time.Duration
	WorkerInterval  time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration

	Seed     bool // demo catalogue and development passwords for staff accounts
	LogLevel slog.Level

	Auth     Auth
	Admin    Account
	Provider Account
}

type Auth struct {
	JWTSecret          []byte
	JWTSecretGenerated bool // JWT_SECRET was empty and a random one is used
	Issuer             string
	AccessTokenTTL     time.Duration
	RefreshTokenTTL    time.Duration
	BcryptCost         int
}

// Account is a staff account; it is created on start if Password is set.
type Account struct {
	Login           string
	Password        string
	DefaultPassword bool // the built-in development password is used
}

// Load reads the configuration through getenv (os.Getenv, or a map in tests)
// and reports all invalid values at once.
func Load(getenv func(string) string) (Config, error) {
	l := loader{getenv: getenv}
	cfg := Config{
		DatabaseURL:     l.str("DATABASE_URL", DefaultDatabaseURL),
		DBMaxConns:      int32(l.int("DB_MAX_CONNS", 0, 0, 1000)),
		ListenAddr:      l.str("LISTEN_ADDR", ":8080"),
		ReservationTTL:  l.duration("RESERVATION_TTL", 5*time.Minute),
		PaymentTimeout:  l.duration("PAYMENT_TIMEOUT", 10*time.Minute),
		WorkerInterval:  l.duration("WORKER_INTERVAL", 5*time.Second),
		RequestTimeout:  l.duration("REQUEST_TIMEOUT", 10*time.Second),
		ShutdownTimeout: l.duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		Seed:            l.bool("SEED", true),
		LogLevel:        l.logLevel("LOG_LEVEL", slog.LevelInfo),
		Auth: Auth{
			Issuer:          l.str("JWT_ISSUER", "last-ticket"),
			AccessTokenTTL:  l.duration("ACCESS_TOKEN_TTL", 15*time.Minute),
			RefreshTokenTTL: l.duration("REFRESH_TOKEN_TTL", 7*24*time.Hour),
			BcryptCost:      l.int("BCRYPT_COST", bcrypt.DefaultCost, bcrypt.MinCost, bcrypt.MaxCost),
		},
	}
	cfg.Auth.JWTSecret, cfg.Auth.JWTSecretGenerated = l.secret("JWT_SECRET", 32)
	cfg.Admin = l.account("ADMIN", "admin", cfg.Seed)
	cfg.Provider = l.account("PROVIDER", "provider", cfg.Seed)
	return cfg, errors.Join(l.errs...)
}

type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) fail(key, value, want string) {
	l.errs = append(l.errs, fmt.Errorf("%s=%q: %s", key, value, want))
}

func (l *loader) get(key string) string { return strings.TrimSpace(l.getenv(key)) }

func (l *loader) str(key, def string) string {
	if v := l.get(key); v != "" {
		return v
	}
	return def
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v := l.get(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		l.fail(key, v, "want a positive duration such as 30s, 5m or 1h")
		return def
	}
	return d
}

func (l *loader) int(key string, def, minimum, maximum int) int {
	v := l.get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minimum || n > maximum {
		l.fail(key, v, fmt.Sprintf("want an integer in %d..%d", minimum, maximum))
		return def
	}
	return n
}

func (l *loader) bool(key string, def bool) bool {
	v := l.get(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, v, "want true or false")
		return def
	}
	return b
}

func (l *loader) logLevel(key string, def slog.Level) slog.Level {
	v := l.get(key)
	if v == "" {
		return def
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(v)); err != nil {
		l.fail(key, v, "want debug, info, warn or error")
		return def
	}
	return level
}

// secret returns the configured secret, or a random one if it is unset.
func (l *loader) secret(key string, minLen int) ([]byte, bool) {
	v := l.get(key)
	if v == "" {
		b := make([]byte, minLen)
		_, _ = rand.Read(b)
		return b, true
	}
	if len(v) < minLen {
		l.errs = append(l.errs, fmt.Errorf("%s: must be at least %d bytes", key, minLen))
	}
	return []byte(v), false
}

// account reads <PREFIX>_LOGIN and <PREFIX>_PASSWORD. In seed mode a missing
// password defaults to "<login>-dev-password".
func (l *loader) account(prefix, defLogin string, seed bool) Account {
	a := Account{
		Login:    l.str(prefix+"_LOGIN", defLogin),
		Password: l.getenv(prefix + "_PASSWORD"),
	}
	if a.Password == "" && seed {
		a.Password, a.DefaultPassword = a.Login+"-dev-password", true
	}
	return a
}
