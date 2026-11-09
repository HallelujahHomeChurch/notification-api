package graphfeedback

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

var (
	ErrCursorExpired     = errors.New("Graph feedback cursor expired")
	ErrReaderUnavailable = errors.New("Graph feedback reader unavailable")
)

type Consumer struct {
	Store    *Store
	ReadPage func(context.Context, string) (Page, error)
}

func (c Consumer) Poll(ctx context.Context) (Result, error) {
	if c.Store == nil || c.ReadPage == nil {
		return Result{}, errors.New("Graph feedback requires a verified reader and checkpoint store")
	}
	cursor, err := c.Store.Cursor(ctx)
	if err != nil {
		return Result{}, err
	}
	page, err := c.ReadPage(ctx, cursor.Value)
	if errors.Is(err, ErrCursorExpired) && cursor.Value != "" {
		page, err = c.ReadPage(ctx, "")
	}
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, ErrReaderUnavailable
	}
	return c.Store.ApplyPage(ctx, cursor, page)
}

func (c Consumer) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		result, err := c.Poll(pollCtx)
		cancel()
		if err != nil && !errors.Is(err, ErrCursorMoved) && !errors.Is(err, ErrDeliveryBusy) && !errors.Is(err, ErrReceiptPending) {
			return err
		}
		if result.Unmatched > 0 {
			slog.Warn("graph_feedback_unmatched", "count", result.Unmatched)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
