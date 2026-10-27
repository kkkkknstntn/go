package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/ids"
)

const (
	MinSecretLength = 32 // RFC 7518 §3.2: an HS256 key must be at least 256 bits
	Audience        = "last-ticket-api"
	clockSkew       = 5 * time.Second
)

// TokenManager issues and checks HS256 access tokens. All instances need the
// same secret to accept each other's tokens.
type TokenManager struct {
	secret []byte
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

func NewTokenManager(secret []byte, issuer string, ttl time.Duration) (*TokenManager, error) {
	if len(secret) < MinSecretLength {
		return nil, fmt.Errorf("jwt secret must be at least %d bytes", MinSecretLength)
	}
	if issuer == "" || ttl <= 0 {
		return nil, errors.New("jwt issuer and positive ttl are required")
	}
	return &TokenManager{secret: secret, issuer: issuer, ttl: ttl, now: time.Now}, nil
}

func (m *TokenManager) TTL() time.Duration { return m.ttl }

type accessClaims struct {
	Role domain.Role `json:"role"`
	jwt.RegisteredClaims
}

func (m *TokenManager) Issue(p Principal) (string, error) {
	now := m.now()
	claims := accessClaims{
		Role: p.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   p.UserID,
			Audience:  jwt.ClaimStrings{Audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
			ID:        ids.New(""),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
}

// Verify checks the signature, issuer, audience and expiry. Only HS256 is
// accepted, which rules out "alg": "none" and algorithm substitution.
func (m *TokenManager) Verify(token string) (Principal, error) {
	var c accessClaims
	_, err := jwt.ParseWithClaims(token, &c,
		func(*jwt.Token) (any, error) { return m.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(m.issuer),
		jwt.WithAudience(Audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(clockSkew),
		jwt.WithTimeFunc(m.now),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", domain.ErrInvalidToken, err)
	}
	if c.Subject == "" || !c.Role.Valid() {
		return Principal{}, fmt.Errorf("%w: missing subject or unknown role", domain.ErrInvalidToken)
	}
	return Principal{UserID: c.Subject, Role: c.Role}, nil
}
