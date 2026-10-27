package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"awesomeProject/internal/config"
	"awesomeProject/internal/ids"
	"awesomeProject/internal/testutil/pgtest"
)

type running struct {
	t      *testing.T
	app    *App
	base   string
	stop   context.CancelFunc
	served chan error
}

func start(t *testing.T, shutdownTimeout time.Duration) *running {
	t.Helper()
	pgtest.Pool(t) // skips when there is no database
	cfg, err := config.Load(func(k string) string {
		return map[string]string{
			"DATABASE_URL":     pgtest.URL(),
			"JWT_SECRET":       strings.Repeat("k", 32),
			"BCRYPT_COST":      "4",
			"WORKER_INTERVAL":  "50ms",
			"DB_MAX_CONNS":     "10",
			"SHUTDOWN_TIMEOUT": shutdownTimeout.String(),
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := New(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	r := &running{t: t, app: a, base: "http://" + ln.Addr().String(), stop: stop, served: make(chan error, 1)}
	go func() { r.served <- a.Serve(ctx, ln) }()
	return r
}

func (r *running) post(path, token string, body any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", r.base+path, bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}

// buyerToken signs a buyer up and logs in over HTTP.
func (r *running) buyerToken() string {
	r.t.Helper()
	login := ids.New("b-")
	resp, err := r.post("/users", "", map[string]any{"name": "b", "login": login, "password": "password-1", "balance": 0})
	if err != nil || resp.StatusCode != 201 {
		r.t.Fatalf("register: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, err = r.post("/auth/login", "", map[string]any{"login": login, "password": "password-1"})
	if err != nil || resp.StatusCode != 200 {
		r.t.Fatalf("login: %v %v", resp, err)
	}
	defer resp.Body.Close()
	var pair struct{ AccessToken string }
	_ = json.NewDecoder(resp.Body).Decode(&pair)
	return pair.AccessToken
}

// holdSeat locks a seat row in another transaction, so that a reservation of
// that seat blocks until release is called.
func holdSeat(t *testing.T, seat string) (release func()) {
	t.Helper()
	tx, err := pgtest.Pool(t).Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(),
		`SELECT 1 FROM seats WHERE event_id = $1 AND number = $2 FOR UPDATE`, demoEventID, seat); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback(context.Background()) }
}

func reservationsOf(t *testing.T, seat string) int {
	t.Helper()
	var n int
	if err := pgtest.Pool(t).QueryRow(context.Background(),
		`SELECT count(*) FROM reservations WHERE event_id = $1 AND seat_number = $2 AND status = 'reserved'`,
		demoEventID, seat).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// freeSeat makes sure an earlier test run did not leave the seat reserved.
func freeSeat(t *testing.T, seat string) {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Pool(t)
	if _, err := pool.Exec(ctx, `UPDATE reservations SET status = 'canceled', finished_at = now()
		WHERE event_id = $1 AND seat_number = $2 AND status = 'reserved'`, demoEventID, seat); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE seats SET status = 'available' WHERE event_id = $1 AND number = $2 AND status = 'reserved'`,
		demoEventID, seat); err != nil {
		t.Fatal(err)
	}
}

func TestServesAndStops(t *testing.T) {
	r := start(t, 5*time.Second)
	resp, err := http.Get(r.base + "/readyz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("readyz: %v %v", resp, err)
	}
	resp.Body.Close()
	// The seeded admin can log in with the development password.
	resp, err = r.post("/auth/login", "", map[string]any{"login": "admin", "password": "admin-dev-password"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("admin login: %v %v", resp, err)
	}
	resp.Body.Close()
	r.stop()
	select {
	case err := <-r.served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	if _, err := http.Get(r.base + "/healthz"); err == nil {
		t.Fatal("the server must not accept requests after shutdown")
	}
}

// An in-flight request finishes during a graceful shutdown.
func TestShutdownLetsActiveRequestsFinish(t *testing.T) {
	const seat = "A98"
	freeSeat(t, seat)
	r := start(t, 5*time.Second)
	tok := r.buyerToken()
	release := holdSeat(t, seat)

	result := make(chan int, 1)
	go func() {
		resp, err := r.post("/reservations", tok, map[string]any{"eventId": demoEventID, "seatId": seat})
		if err != nil {
			result <- 0
			return
		}
		resp.Body.Close()
		result <- resp.StatusCode
	}()
	time.Sleep(150 * time.Millisecond) // the request is now waiting on the seat lock
	r.stop()
	time.Sleep(100 * time.Millisecond)
	release()

	if code := <-result; code != http.StatusCreated {
		t.Fatalf("in-flight request: status %d, want 201", code)
	}
	if err := <-r.served; err != nil {
		t.Fatal(err)
	}
	if n := reservationsOf(t, seat); n != 1 {
		t.Fatalf("reservations: %d", n)
	}
	freeSeat(t, seat)
}

// A request still running after the shutdown timeout is cancelled and its
// transaction rolled back.
func TestShutdownTimeoutCancelsRequests(t *testing.T) {
	const seat = "A99"
	freeSeat(t, seat)
	r := start(t, 200*time.Millisecond)
	tok := r.buyerToken()
	release := holdSeat(t, seat)
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := r.post("/reservations", tok, map[string]any{"eventId": demoEventID, "seatId": seat}); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(150 * time.Millisecond)
	began := time.Now()
	r.stop()
	if err := <-r.served; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("shutdown took %s", took)
	}
	<-done
	release()
	if n := reservationsOf(t, seat); n != 0 {
		t.Fatalf("the cancelled request must leave nothing behind, found %d reservations", n)
	}
}
