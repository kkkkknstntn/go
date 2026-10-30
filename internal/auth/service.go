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
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/ids"
	"awesomeProject/internal/postgres"
)

type Config struct {
	RefreshTTL time.Duration
	BcryptCost int
}

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
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshToken     string `json:"refreshToken"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
}

type Registration struct {
	Name     string
	Login    string
	Password string
	Balance  int64
}

var loginPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]{3,64}$`)

func validateAccount(name, login, password string) error {
	var errs []error
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 200 {
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

func (s *Service) Register(ctx context.Context, r Registration) (*domain.User, error) {
	if err := validateAccount(r.Name, r.Login, r.Password); err != nil {
		return nil, err
	}
	if r.Balance < 0 || r.Balance > domain.MaxAmount {
		return nil, fmt.Errorf("%w: balance must be in 0..%d", domain.ErrValidation, domain.MaxAmount)
	}
	return s.createAccount(ctx, r.Name, r.Login, r.Password, domain.RoleUser, r.Balance)
}

func (s *Service) EnsureAccount(ctx context.Context, name, login, password string, role domain.Role) (bool, error) {
	if !role.Valid() {
		return false, fmt.Errorf("%w: unknown role %q", domain.ErrValidation, role)
	}
	if err := validateAccount(name, login, password); err != nil {
		return false, err
	}
	if exists, err := s.checkRoleOfLogin(ctx, login, role); exists || err != nil {
		return false, err
	}
	_, err := s.createAccount(ctx, name, login, password, role, 0)
	if errors.Is(err, domain.ErrAlreadyExists) {
		_, err = s.checkRoleOfLogin(ctx, login, role)
		return false, err
	}
	return err == nil, err
}

func (s *Service) checkRoleOfLogin(ctx context.Context, login string, want domain.Role) (bool, error) {
	var role domain.Role
	err := s.pool.QueryRow(ctx, `SELECT role FROM users WHERE lower(login) = lower($1)`, login).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if role != want {
		return true, fmt.Errorf("login %q is already used by a %s account", login, role)
	}
	return true, nil
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

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
