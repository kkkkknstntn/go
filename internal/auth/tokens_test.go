package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"awesomeProject/internal/domain"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func newTestManager(t *testing.T) *TokenManager {
	t.Helper()
	m, err := NewTokenManager(testSecret, "test-issuer", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestIssueAndVerify(t *testing.T) {
	m := newTestManager(t)
	token, err := m.Issue(Principal{UserID: "user-1", Role: domain.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != "user-1" || p.Role != domain.RoleUser {
		t.Fatalf("got %+v", p)
	}
}

func TestVerifyRejects(t *testing.T) {
	m := newTestManager(t)
	good, _ := m.Issue(Principal{UserID: "user-1", Role: domain.RoleUser})

	other, _ := NewTokenManager([]byte("another-secret-another-secret-123"), "test-issuer", time.Minute)
	foreignSigned, _ := other.Issue(Principal{UserID: "user-1", Role: domain.RoleUser})

	otherIssuer, _ := NewTokenManager(testSecret, "someone-else", time.Minute)
	foreignIssuer, _ := otherIssuer.Issue(Principal{UserID: "user-1", Role: domain.RoleUser})

	expiredManager := newTestManager(t)
	expiredManager.now = func() time.Time { return time.Now().Add(-time.Hour) }
	expired, _ := expiredManager.Issue(Principal{UserID: "user-1", Role: domain.RoleUser})

	// "alg": "none" must never be accepted.
	unsigned, _ := jwt.NewWithClaims(jwt.SigningMethodNone, accessClaims{
		Role:             domain.RoleAdmin,
		RegisteredClaims: jwt.RegisteredClaims{Subject: "user-1", Issuer: "test-issuer", Audience: jwt.ClaimStrings{Audience}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)

	// A valid token whose payload was edited to claim the admin role: the
	// original signature no longer matches.
	parts := strings.Split(good, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	forged := strings.Replace(string(payload), `"role":"user"`, `"role":"admin"`, 1)
	if forged == string(payload) {
		t.Fatal("test setup: role claim not found")
	}
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(forged)) + "." + parts[2]

	unknownRole, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		Role:             "root",
		RegisteredClaims: jwt.RegisteredClaims{Subject: "user-1", Issuer: "test-issuer", Audience: jwt.ClaimStrings{Audience}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString(testSecret)

	noExpiry, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		Role:             domain.RoleUser,
		RegisteredClaims: jwt.RegisteredClaims{Subject: "user-1", Issuer: "test-issuer", Audience: jwt.ClaimStrings{Audience}},
	}).SignedString(testSecret)

	for name, token := range map[string]string{
		"garbage":        "not-a-token",
		"empty":          "",
		"foreign secret": foreignSigned,
		"foreign issuer": foreignIssuer,
		"expired":        expired,
		"alg none":       unsigned,
		"tampered":       tampered,
		"unknown role":   unknownRole,
		"no expiry":      noExpiry,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Verify(token); !errors.Is(err, domain.ErrInvalidToken) {
				t.Fatalf("want ErrInvalidToken, got %v", err)
			}
		})
	}
}

func TestNewTokenManagerRequiresStrongSecret(t *testing.T) {
	if _, err := NewTokenManager([]byte("short"), "iss", time.Minute); err == nil {
		t.Fatal("a short secret must be rejected")
	}
}

func TestPrincipal(t *testing.T) {
	user := Principal{UserID: "u1", Role: domain.RoleUser}
	adminP := Principal{UserID: "a1", Role: domain.RoleAdmin}
	if !user.CanAccess("u1") || user.CanAccess("u2") {
		t.Fatal("users see only their own objects")
	}
	if !adminP.CanAccess("u2") {
		t.Fatal("admins see everything")
	}
	if !user.HasRole(domain.RoleUser, domain.RoleAdmin) || user.HasRole(domain.RoleProvider) {
		t.Fatal("HasRole")
	}
}
