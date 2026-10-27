// Package worker periodically stores expired reservations and timed-out
// payments. Business operations check deadlines themselves, so a late sweeper
// only delays the cleanup.
package worker

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"
)

// Sweeper is implemented by *booking.Service.
type Sweeper interface {
	ExpireDueReservations(ctx context.Context) (int, error)
	TimeoutDuePayments(ctx context.Context) (int, error)
}

// Run sweeps right away and then every interval until ctx is cancelled.
func Run(ctx context.Context, s Sweeper, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		sweepOnce(ctx, s, log)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func sweepOnce(ctx context.Context, s Sweeper, log *slog.Logger) {
	report(ctx, log, "reservations expired", s.ExpireDueReservations)
	report(ctx, log, "payments timed out", s.TimeoutDuePayments)
}

func report(ctx context.Context, log *slog.Logger, what string, sweep func(context.Context) (int, error)) {
	// An unrecovered panic in this goroutine would kill the whole process,
	// HTTP server included. The pass is abandoned instead; the open
	// transaction has already been rolled back while the stack unwound.
	defer func() {
		if v := recover(); v != nil {
			log.ErrorContext(ctx, "sweeper: panic", "what", what, "panic", v, "stack", string(debug.Stack()))
		}
	}()
	n, err := sweep(ctx)
	if n > 0 {
		log.InfoContext(ctx, "sweeper: "+what, "count", n)
	}
	if err != nil && ctx.Err() == nil {
		log.ErrorContext(ctx, "sweeper: "+what+" with errors", "err", err)
	}
}
