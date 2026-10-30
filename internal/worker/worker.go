package worker

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"
)

type Sweeper interface {
	ExpireDueReservations(ctx context.Context) (int, error)
	TimeoutDuePayments(ctx context.Context) (int, error)
}

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
