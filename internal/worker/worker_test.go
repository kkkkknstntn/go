package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSweeper struct {
	reservations, payments atomic.Int32
}

func (f *fakeSweeper) ExpireDueReservations(context.Context) (int, error) {
	f.reservations.Add(1)
	return 1, nil
}

func (f *fakeSweeper) TimeoutDuePayments(context.Context) (int, error) {
	f.payments.Add(1)
	return 0, errors.New("boom") // errors are logged, the loop goes on
}

type panickingSweeper struct{ fakeSweeper }

func (p *panickingSweeper) ExpireDueReservations(context.Context) (int, error) {
	p.reservations.Add(1)
	panic("bug in a sweep")
}

func TestPanicDoesNotStopTheSweeper(t *testing.T) {
	s := &panickingSweeper{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, s, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deadline := time.Now().Add(2 * time.Second)
	for s.payments.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.reservations.Load() < 3 || s.payments.Load() < 3 {
		t.Fatalf("the loop stopped after a panic: reservations=%d payments=%d", s.reservations.Load(), s.payments.Load())
	}
}

func TestRunSweepsPeriodicallyAndStops(t *testing.T) {
	s := &fakeSweeper{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, s, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for s.payments.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if s.reservations.Load() < 3 || s.payments.Load() < 3 {
		t.Fatalf("passes: reservations=%d payments=%d", s.reservations.Load(), s.payments.Load())
	}
}
