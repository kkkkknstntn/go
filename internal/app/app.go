package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"awesomeProject/internal/auth"
	"awesomeProject/internal/booking"
	"awesomeProject/internal/config"
	"awesomeProject/internal/httpapi"
	"awesomeProject/internal/postgres"
	"awesomeProject/internal/worker"
)

type App struct {
	cfg     config.Config
	log     *slog.Logger
	pool    *pgxpool.Pool
	booking *booking.Service
	handler http.Handler
}

func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return nil, err
	}
	a, err := build(ctx, cfg, log, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return a, nil
}

func build(ctx context.Context, cfg config.Config, log *slog.Logger, pool *pgxpool.Pool) (*App, error) {
	if err := postgres.Migrate(ctx, pool); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	bookingSvc, err := booking.New(pool, booking.Config{
		ReservationTTL:   cfg.ReservationTTL,
		PaymentTimeout:   cfg.PaymentTimeout,
		SweepItemTimeout: cfg.RequestTimeout,
	})
	if err != nil {
		return nil, err
	}
	tokens, err := auth.NewTokenManager(cfg.Auth.JWTSecret, cfg.Auth.Issuer, cfg.Auth.AccessTokenTTL)
	if err != nil {
		return nil, err
	}
	authSvc, err := auth.NewService(pool, tokens, auth.Config{
		RefreshTTL: cfg.Auth.RefreshTokenTTL,
		BcryptCost: cfg.Auth.BcryptCost,
	})
	if err != nil {
		return nil, err
	}
	if cfg.Auth.JWTSecretGenerated {
		log.Warn("JWT_SECRET is not set, using a random one: tokens will not survive a restart or work on another instance")
	}
	if err := ensureStaffAccounts(ctx, cfg, log, authSvc); err != nil {
		return nil, err
	}
	if cfg.Seed {
		if err := seedCatalog(ctx, bookingSvc); err != nil {
			return nil, fmt.Errorf("seed: %w", err)
		}
	}
	handler := httpapi.NewRouter(httpapi.Deps{
		Booking:        bookingSvc,
		Auth:           authSvc,
		Logger:         log,
		Ready:          pool.Ping,
		RequestTimeout: cfg.RequestTimeout,
	})
	return &App{cfg: cfg, log: log, pool: pool, booking: bookingSvc, handler: handler}, nil
}

func (a *App) Close() { a.pool.Close() }

func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	requestCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      a.cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}

	workerCtx, stopWorker := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWorker()
	var wg sync.WaitGroup
	wg.Go(func() { worker.Run(workerCtx, a.booking, a.cfg.WorkerInterval, a.log) })

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	a.log.Info("listening", "addr", ln.Addr().String(), "reservation_ttl", a.cfg.ReservationTTL,
		"payment_timeout", a.cfg.PaymentTimeout, "docs", "/docs")

	select {
	case err := <-serveErr:
		stopWorker()
		wg.Wait()
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	a.log.Info("shutting down", "timeout", a.cfg.ShutdownTimeout)
	stopWorker()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		a.log.Warn("shutdown timeout reached, cancelling in-flight requests")
		cancelRequests()
		err = srv.Close()
	}
	wg.Wait()
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	a.log.Info("stopped")
	return nil
}
