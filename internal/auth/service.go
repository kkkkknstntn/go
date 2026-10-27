package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/ids"
	"awesomeProject/internal/postgres"
)

type Config struct {
	RefreshTTL time.Duration
	BcryptCost int // 0: bcrypt.DefaultCost
}

// Service manages accounts and sessions. A session is a short-lived JWT
// access token plus a refresh token that is replaced on every refresh. If a
// replaced refresh token shows up again it has been copied, and all sessions
// of the user are revoked.
type Service struct {
	pool       *pgxpool.Pool
	tokens     *TokenManager
	hasher     *hasher
	refreshTTL time.Duration
}

func NewService(pool *pgxpool.Pool, tokens *TokenManager, cfg Config) (*Service, error) {
	if cfg.RefreshTTL <= 0 {
		return nil, errors.New("auth: refresh ttl must be positive")
	}
	h, err := newHasher(cfg.BcryptCost)
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, tokens: tokens, hasher: h, refreshTTL: cfg.RefreshTTL}, nil
}

type TokenPair struct {
	AccessToken      string `json:"accessToken"`
	TokenType        string `json:"tokenType"`
	ExpiresIn        int64  `json:"expiresIn"` // seconds
	RefreshToken     string `json:"refreshToken"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"` // seconds
}

// Registration is a buyer signing up with a test balance.
type Registration struct {
	Name     string
	Login    string
	Password string
	Balance  int64
}

var loginPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]{3,64}$`)

func validateAccount(name, login, password string) error {
	var errs []error
	if strings.TrimSpace(name) == "" || len(name) > 200 {
		errs = append(errs, fmt.Errorf("%w: name must be 1..200 characters", domain.ErrValidation))
	}
	if !loginPattern.MatchString(login) {
		errs = append(errs, fmt.Errorf("%w: login must match %s", domain.ErrValidation, loginPattern))
	}
	if err := validatePassword(password); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Register creates a buyer account (role user).
func (s *Service) Register(ctx context.Context, r Registration) (*domain.User, error) {
	if err := validateAccount(r.Name, r.Login, r.Password); err != nil {
		return nil, err
	}
	if r.Balance < 0 || r.Balance > domain.MaxAmount {
		return nil, fmt.Errorf("%w: balance must be in 0..%d", domain.ErrValidation, domain.MaxAmount)
	}
	return s.createAccount(ctx, r.Name, r.Login, r.Password, domain.RoleUser, r.Balance)
}

// EnsureAccount creates a staff account unless the login is taken and reports
// whether it did.
func (s *Service) EnsureAccount(ctx context.Context, name, login, password string, role domain.Role) (bool, error) {
	if !role.Valid() {
		return false, fmt.Errorf("%w: unknown role %q", domain.ErrValidation, role)
	}
	if err := validateAccount(name, login, password); err != nil {
		return false, err
	}
	_, err := s.createAccount(ctx, name, login, password, role, 0)
	if errors.Is(err, domain.ErrAlreadyExists) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) createAccount(ctx context.Context, name, login, password string, role domain.Role, balance int64) (*domain.User, error) {
	hash, err := s.hasher.hash(password)
	if err != nil {
		return nil, err
	}
	u := domain.User{ID: ids.New("user-"), Name: name, Login: login, Role: role, Balance: balance}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO users (id, name, login, password_hash, role, balance)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
		u.ID, u.Name, u.Login, hash, u.Role, u.Balance).Scan(&u.CreatedAt)
	if postgres.IsUniqueViolation(err) {
		return nil, fmt.Errorf("%w: login %q is taken", domain.ErrAlreadyExists, login)
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Login checks the password and opens a session.
func (s *Service) Login(ctx context.Context, login, password string) (*TokenPair, error) {
	var (
		id, hash string
		role     domain.Role
	)
	err := s.pool.QueryRow(ctx, `SELECT id, password_hash, role FROM users WHERE lower(login) = lower($1)`, login).
		Scan(&id, &hash, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		s.hasher.burn(password)
		return nil, domain.ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if !s.hasher.verify(hash, password) {
		return nil, domain.ErrInvalidCredentials
	}
	pair, _, err := s.openSession(ctx, s.pool, Principal{UserID: id, Role: role})
	return pair, err
}

// Refresh exchanges a refresh token for a new pair and revokes the old token.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("%w: refresh token is required", domain.ErrInvalidToken)
	}
	var (
		pair   *TokenPair
		reused bool
	)
	err := postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			id, userID       string
			role             domain.Role
			expired, revoked bool
		)
		err := tx.QueryRow(ctx, `
			SELECT t.id, t.user_id, u.role, t.expires_at <= clock_timestamp(), t.revoked_at IS NOT NULL
			FROM refresh_tokens t JOIN users u ON u.id = t.user_id
			WHERE t.token_hash = $1
			FOR UPDATE OF t`, hashToken(refreshToken)).Scan(&id, &userID, &role, &expired, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: unknown refresh token", domain.ErrInvalidToken)
		}
		if err != nil {
			return err
		}
		if revoked {
			// A replaced or logged-out token came back, so someone has a copy.
			reused = true
			return s.revokeAll(ctx, tx, userID)
		}
		if expired {
			return fmt.Errorf("%w: refresh token expired", domain.ErrInvalidToken)
		}
		var newID string
		pair, newID, err = s.openSession(ctx, tx, Principal{UserID: userID, Role: role})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = clock_timestamp(), replaced_by = $2 WHERE id = $1`, id, newID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if reused {
		return nil, fmt.Errorf("%w: refresh token was already used; all sessions revoked", domain.ErrInvalidToken)
	}
	return pair, nil
}

// Logout revokes the refresh token; unknown or revoked tokens are ignored.
// The access token stays valid until it expires, hence its short lifetime.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = clock_timestamp() WHERE token_hash = $1 AND revoked_at IS NULL`,
		hashToken(refreshToken))
	return err
}

func (s *Service) Authenticate(accessToken string) (Principal, error) {
	return s.tokens.Verify(accessToken)
}

func (s *Service) revokeAll(ctx context.Context, q postgres.Querier, userID string) error {
	_, err := q.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = clock_timestamp() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// openSession issues an access token and a refresh token and returns the id
// of the stored refresh token.
func (s *Service) openSession(ctx context.Context, q postgres.Querier, p Principal) (*TokenPair, string, error) {
	access, err := s.tokens.Issue(p)
	if err != nil {
		return nil, "", err
	}
	refresh := rand.Text()
	id := ids.New("rt-")
	if _, err := q.Exec(ctx, `
		INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, clock_timestamp() + make_interval(secs => $4))`,
		id, p.UserID, hashToken(refresh), s.refreshTTL.Seconds()); err != nil {
		return nil, "", err
	}
	return &TokenPair{
		AccessToken:      access,
		TokenType:        "Bearer",
		ExpiresIn:        int64(s.tokens.TTL() / time.Second),
		RefreshToken:     refresh,
		RefreshExpiresIn: int64(s.refreshTTL / time.Second),
	}, id, nil
}

// hashToken is what the database stores. SHA-256 without salt is enough: the
// token is random (rand.Text, 128+ bits), there is nothing to brute-force.
func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
