package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"awesomeProject/internal/service"
)

var handler http.Handler

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5432/tickets?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: PostgreSQL is not available (%v)\n", err)
		os.Exit(0)
	}
	if err := service.Migrate(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	handler = New(service.New(pool, service.Config{ReservationTTL: time.Minute, PaymentTimeout: time.Minute}))
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func call(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd bytes.Buffer
	if body != nil {
		json.NewEncoder(&rd).Encode(body)
	}
	req := httptest.NewRequest(method, path, &rd)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestFullFlowOverHTTP(t *testing.T) {
	code, org := call(t, "POST", "/organizers", map[string]any{"name": "o", "balance": 50000})
	if code != 201 {
		t.Fatal(code, org)
	}
	code, ev := call(t, "POST", "/events", map[string]any{
		"name": "e", "organizerId": org["id"], "generate": map[string]any{"prefix": "A", "count": 3, "price": 2000}})
	if code != 201 {
		t.Fatal(code, ev)
	}
	_, user := call(t, "POST", "/users", map[string]any{"name": "Ivan", "balance": 10000})
	_, user2 := call(t, "POST", "/users", map[string]any{"name": "Petr", "balance": 10000})

	res := map[string]any{"userId": user["id"], "eventId": ev["id"], "seatId": "A1"}
	code, r := call(t, "POST", "/reservations", res)
	if code != 201 || r["status"] != "reserved" {
		t.Fatal(code, r)
	}
	res2 := map[string]any{"userId": user2["id"], "eventId": ev["id"], "seatId": "A1"}
	if code, _ := call(t, "POST", "/reservations", res2); code != http.StatusConflict {
		t.Fatalf("want 409, got %d", code)
	}

	code, p := call(t, "POST", fmt.Sprintf("/reservations/%v/purchase", r["reservationId"]), map[string]any{"userId": user["id"]})
	if code != 201 || p["amount"].(float64) != 2000 || p["status"] != "payment_pending" {
		t.Fatal(code, p)
	}
	for i := 0; i < 2; i++ { // second call is a duplicate
		code, pay := call(t, "POST", fmt.Sprintf("/payments/%v/confirm", p["paymentId"]), nil)
		if code != 200 || pay["status"] != "succeeded" {
			t.Fatal(code, pay)
		}
	}
	_, u := call(t, "GET", fmt.Sprintf("/users/%v", user["id"]), nil)
	_, o := call(t, "GET", fmt.Sprintf("/organizers/%v", org["id"]), nil)
	if u["balance"].(float64) != 8000 || o["balance"].(float64) != 52000 {
		t.Fatal(u, o)
	}
	if code, _ := call(t, "POST", fmt.Sprintf("/reservations/%v/cancel", r["reservationId"]), nil); code != http.StatusConflict {
		t.Fatalf("cancel purchased: want 409, got %d", code)
	}
	if code, _ := call(t, "GET", "/users/nope", nil); code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
	// the client cannot choose the price: unknown fields are rejected
	if code, _ := call(t, "POST", "/reservations", map[string]any{"userId": "x", "eventId": "y", "seatId": "A2", "price": 1}); code != 400 {
		t.Fatalf("want 400, got %d", code)
	}
}

func TestInsufficientFundsOverHTTP(t *testing.T) {
	_, org := call(t, "POST", "/organizers", map[string]any{"name": "o", "balance": 0})
	_, ev := call(t, "POST", "/events", map[string]any{"name": "e", "organizerId": org["id"], "seats": []map[string]any{{"number": "A1", "price": 2000}}})
	_, user := call(t, "POST", "/users", map[string]any{"name": "poor", "balance": 500})
	_, r := call(t, "POST", "/reservations", map[string]any{"userId": user["id"], "eventId": ev["id"], "seatId": "A1"})
	_, p := call(t, "POST", fmt.Sprintf("/reservations/%v/purchase", r["reservationId"]), map[string]any{"userId": user["id"]})
	code, out := call(t, "POST", fmt.Sprintf("/payments/%v/confirm", p["paymentId"]), nil)
	if code != http.StatusPaymentRequired || out["error"] != "insufficient_funds" {
		t.Fatal(code, out)
	}
	if pay := out["payment"].(map[string]any); pay["status"] != "failed" {
		t.Fatal(pay)
	}
	_, seats := (func() (int, []map[string]any) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/events/%v/seats", ev["id"]), nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var s []map[string]any
		json.Unmarshal(rec.Body.Bytes(), &s)
		return rec.Code, s
	})()
	if seats[0]["status"] != "available" {
		t.Fatal(seats)
	}
}

func TestConcurrentReserveOverHTTP(t *testing.T) {
	_, org := call(t, "POST", "/organizers", map[string]any{"name": "o", "balance": 0})
	_, ev := call(t, "POST", "/events", map[string]any{"name": "e", "organizerId": org["id"], "seats": []map[string]any{{"number": "A17", "price": 2000}}})
	var ok, conflict atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		_, u := call(t, "POST", "/users", map[string]any{"name": "u", "balance": 0})
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := call(t, "POST", "/reservations", map[string]any{"userId": u["id"], "eventId": ev["id"], "seatId": "A17"})
			switch code {
			case 201:
				ok.Add(1)
			case 409:
				conflict.Add(1)
			default:
				t.Error(code)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || conflict.Load() != 99 {
		t.Fatalf("ok=%d conflict=%d", ok.Load(), conflict.Load())
	}
}

func TestOpenAPIIsValidJSON(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(openapiSpec, &v); err != nil {
		t.Fatal(err)
	}
}
