package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"awesomeProject/internal/httpapi"
	"awesomeProject/internal/service"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Fatalf("invalid %s=%q: want a positive duration like 5m or 30s", key, v)
	}
	return d
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	dbURL := env("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/tickets?sslmode=disable")
	addr := env("LISTEN_ADDR", ":8080")
	cfg := service.Config{
		ReservationTTL: envDuration("RESERVATION_TTL", 5*time.Minute),
		PaymentTimeout: envDuration("PAYMENT_TIMEOUT", 10*time.Minute),
	}
	workerInterval := envDuration("WORKER_INTERVAL", 5*time.Second)
	shutdownTimeout := envDuration("SHUTDOWN_TIMEOUT", 15*time.Second)

	// sigCtx is cancelled on SIGINT/SIGTERM and only drives the shutdown sequence;
	// request contexts are deliberately not derived from it so that in-flight
	// requests can finish within shutdownTimeout.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return fmt.Errorf("DATABASE_URL: %w", err)
	}
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("invalid DB_MAX_CONNS=%q", v)
		}
		poolCfg.MaxConns = int32(n)
	}
	pool, err := pgxpool.NewWithConfig(sigCtx, poolCfg)
	if err != nil {
		return err
	}
	defer pool.Close() // step 5: close DB connections

	if err := pool.Ping(sigCtx); err != nil {
		return fmt.Errorf("database is unreachable: %w", err)
	}
	if err := service.Migrate(sigCtx, pool); err != nil {
		return err
	}
	svc := service.New(pool, cfg)
	if env("SEED", "true") == "true" {
		if err := seed(sigCtx, svc); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}

	// Background workers.
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		svc.RunWorkers(workerCtx, workerInterval, log.Printf)
	}()

	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New(svc),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("listening on %s (reservation TTL %s, payment timeout %s, swagger: /docs)", addr, cfg.ReservationTTL, cfg.PaymentTimeout)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		stopWorkers()
		workers.Wait()
		return err
	case <-sigCtx.Done():
	}

	log.Printf("shutting down")
	// 1+3: stop accepting new requests and let active ones finish within the timeout.
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("forced close, in-flight requests are cancelled and their transactions rolled back: %v", err)
		srv.Close()
	}
	// 2: stop workers and wait for the current pass to finish.
	stopWorkers()
	workers.Wait()
	log.Printf("bye")
	return nil // pool.Close() runs via defer
}

// seed creates a demo organizer and event once, so the API is usable right away.
func seed(ctx context.Context, svc *service.Service) error {
	if _, err := svc.CreateOrganizer(ctx, "concert-company", "Concert Company", 50000); err != nil && !errors.Is(err, service.ErrAlreadyExists) {
		return err
	}
	var seats []service.SeatSpec
	for i := 1; i <= 100; i++ {
		price := int64(2000)
		if i > 80 {
			price = 2500
		}
		seats = append(seats, service.SeatSpec{Number: fmt.Sprintf("A%d", i), Price: price})
	}
	if _, err := svc.CreateEvent(ctx, "event-1", "Concert", "concert-company", seats); err != nil && !errors.Is(err, service.ErrAlreadyExists) {
		return err
	}
	return nil
}
