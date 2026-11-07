// Package dependencyhealth observes dependencies without changing AP readiness.
package dependencyhealth

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type Check struct {
	Name string
	Ping func(context.Context) error
}

// Run is owned and joined by the runtime. Checks never overlap or gate requests.
func Run(ctx context.Context, checks ...Check) { run(ctx, false, checks...) }

// RunAP preserves probe cadence while emitting transitions and five-minute heartbeats.
func RunAP(ctx context.Context, checks ...Check) { run(ctx, true, checks...) }

func run(ctx context.Context, throttle bool, checks ...Check) {
	states := make([]observation, len(checks))
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	defer slog.Info("dependency_monitor_stopped")
	for {
		for i, check := range checks {
			if ctx.Err() != nil {
				return
			}
			var state *observation
			if throttle {
				state = &states[i]
			}
			sample(ctx, check, state)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func sample(ctx context.Context, check Check, state *observation) {
	started := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := check.Ping(probeCtx)
	outcome := "ok"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		outcome = "deadline"
	case errors.Is(err, context.Canceled):
		outcome = "cancelled"
	case err != nil:
		outcome = "unavailable"
	}
	if !state.due(outcome, time.Now()) {
		return
	}
	level := slog.LevelInfo
	if err != nil && ctx.Err() == nil {
		level = slog.LevelWarn
	}
	// Raw dependency errors can contain credentials, query text or provider payloads.
	slog.Log(ctx, level, "dependency_health", "dependency", check.Name, "outcome", outcome, "elapsed_ms", time.Since(started).Milliseconds())
}

// observation belongs to one dependency in the sequential monitor loop.
type observation struct {
	outcome string
	emitted time.Time
}

func (s *observation) due(outcome string, now time.Time) bool {
	if s == nil {
		return true
	}
	if s.emitted.IsZero() || s.outcome != outcome || now.Sub(s.emitted) >= 5*time.Minute {
		s.outcome, s.emitted = outcome, now
		return true
	}
	return false
}
