// Package dependencyhealth observes dependencies without changing AP readiness.
package dependencyhealth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

type Check struct {
	Name string
	Ping func(context.Context) error
}

var errNotConfigured = errors.New("not configured")

// Dapr checks the local sidecar only, not downstream application availability.
func Dapr(port, token string) Check {
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return Check{Name: "dapr_outbound", Ping: func(ctx context.Context) error {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errNotConfigured
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1.0/healthz/outbound", n), nil)
		if err != nil {
			return err
		}
		req.Header.Set("dapr-api-token", token)
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			return errors.New("sidecar unavailable")
		}
		return nil
	}}
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
	case errors.Is(err, errNotConfigured):
		outcome = "not_configured"
	case err != nil:
		outcome = "unavailable"
	}
	if !state.due(outcome, time.Now()) {
		return
	}
	level := slog.LevelInfo
	if err != nil && outcome != "not_configured" && ctx.Err() == nil {
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
