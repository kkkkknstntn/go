package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"awesomeProject/internal/domain"
	"awesomeProject/internal/ids"
	"awesomeProject/internal/testutil/pgtest"
)

var ctx = context.Background()

func newTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := NewService(pgtest.Pool(t), newTestManager(t), Config{RefreshTTL: time.Hour, BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func register(t *testing.T, svc *Service, password string) (*domain.User, string) {
	t.Helper()
	login := ids.New("login-")
	u, err := svc.Register(ctx, Registration{Name: "Ivan", Login: login, Password: password, Balance: 10000})
	if err != nil {
		t.Fatal(err)
	}
	return u, login
}

func TestRegisterAndLogin(t *testing.T) {
	svc := newTestService(t)
	u, login := register(t, svc, "correct-horse")
	if u.Role != domain.RoleUser || u.Balance != 10000 {
		t.Fatalf("got %+v", u)
	}
	var stored string
	if err := pgtest.Pool(t).QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, u.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "correct-horse" {
		t.Fatal("password stored in clear text")
	}

	pair, err := svc.Login(ctx, login, "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	if pair.TokenType != "Bearer" || pair.AccessToken == "" || pair.RefreshToken == "" || pair.ExpiresIn != 15*60 {
		t.Fatalf("got %+v", pair)
	}
	p, err := svc.Authenticate(pair.AccessToken)
	if err != nil || p.UserID != u.ID || p.Role != domain.RoleUser {
		t.Fatalf("authenticate: %+v, %v", p, err)
	}

	// Logins are case-insensitive and unique.
	if _, err := svc.Login(ctx, "  "+login, "correct-horse"); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("a login with spaces is a different login: %v", err)
	}
	_, err = svc.Register(ctx, Registration{Name: "x", Login: u.Login, Password: "another-pass", Balance: 0})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate login: %v", err)
	}
}

func TestLoginFailures(t *testing.T) {
	svc := newTestService(t)
	_, login := register(t, svc, "correct-horse")
	for name, c := range map[string][2]string{
		"wrong password": {login, "wrong-password"},
		"unknown login":  {"nobody-" + login, "correct-horse"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Login(ctx, c[0], c[1]); !errors.Is(err, domain.ErrInvalidCredentials) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestRegisterValidation(t *testing.T) {
	svc := newTestService(t)
	for name, r := range map[string]Registration{
		"short password":   {Name: "a", Login: "valid-login", Password: "short", Balance: 1},
		"long password":    {Name: "a", Login: "valid-login", Password: string(make([]byte, 73)), Balance: 1},
		"bad login":        {Name: "a", Login: "no spaces", Password: "long-enough", Balance: 1},
		"empty name":       {Name: " ", Login: "valid-login", Password: "long-enough", Balance: 1},
		"negative balance": {Name: "a", Login: "valid-login", Password: "long-enough", Balance: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Register(ctx, r); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestRefreshRotation(t *testing.T) {
	svc := newTestService(t)
	_, login := register(t, svc, "correct-horse")
	first, err := svc.Login(ctx, login, "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("the refresh token must rotate")
	}
	if _, err := svc.Authenticate(second.AccessToken); err != nil {
		t.Fatal(err)
	}

	// Replaying the rotated token is treated as theft: every session dies.
	if _, err := svc.Refresh(ctx, first.RefreshToken); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := svc.Refresh(ctx, second.RefreshToken); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("after reuse detection the newest token must be revoked too: %v", err)
	}
	if _, err := svc.Refresh(ctx, "unknown"); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("unknown: %v", err)
	}
}

// Two concurrent refreshes with the same token: exactly one wins.
func TestConcurrentRefresh(t *testing.T) {
	svc := newTestService(t)
	_, login := register(t, svc, "correct-horse")
	pair, err := svc.Login(ctx, login, "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Go(func() { _, results[i] = svc.Refresh(ctx, pair.RefreshToken) })
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		} else if !errors.Is(err, domain.ErrInvalidToken) {
			t.Fatal(err)
		}
	}
	if ok != 1 {
		t.Fatalf("want exactly one successful refresh, got %d", ok)
	}
}

func TestRefreshExpired(t *testing.T) {
	svc := newTestService(t)
	svc.refreshTTL = 20 * time.Millisecond
	_, login := register(t, svc, "correct-horse")
	pair, err := svc.Login(ctx, login, "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := svc.Refresh(ctx, pair.RefreshToken); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestLogout(t *testing.T) {
	svc := newTestService(t)
	_, login := register(t, svc, "correct-horse")
	pair, err := svc.Login(ctx, login, "correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, pair.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, pair.RefreshToken); err != nil {
		t.Fatal("logout must be idempotent:", err)
	}
	if _, err := svc.Refresh(ctx, pair.RefreshToken); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("refresh after logout: %v", err)
	}
}

func TestEnsureAccount(t *testing.T) {
	svc := newTestService(t)
	login := ids.New("admin-")
	created, err := svc.EnsureAccount(ctx, "Admin", login, "admin-password", domain.RoleAdmin)
	if err != nil || !created {
		t.Fatalf("first call: %v, %v", created, err)
	}
	created, err = svc.EnsureAccount(ctx, "Admin", login, "other-password", domain.RoleAdmin)
	if err != nil || created {
		t.Fatalf("second call must keep the existing account: %v, %v", created, err)
	}
	pair, err := svc.Login(ctx, login, "admin-password")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := svc.Authenticate(pair.AccessToken)
	if p.Role != domain.RoleAdmin {
		t.Fatalf("role: %s", p.Role)
	}
}
