package dependencyhealth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSampleClassifiesWithoutRawError(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	for _, tc := range []struct {
		err     error
		outcome string
	}{
		{nil, "ok"}, {context.DeadlineExceeded, "deadline"}, {context.Canceled, "cancelled"}, {errors.New("secret postgres://token@host/query"), "unavailable"},
	} {
		logs.Reset()
		sample(context.Background(), Check{Name: "db", Ping: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 2*time.Second {
				t.Fatal("missing bound")
			}
			return tc.err
		}}, nil)
		if !strings.Contains(logs.String(), "outcome="+tc.outcome) || strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "postgres://") {
			t.Fatal(logs.String())
		}
	}
}

func TestRunCancelsAndDoesNotStartAnotherProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, Check{Name: "db", Ping: func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }}, Check{Name: "redis", Ping: func(context.Context) error { t.Error("probe started after cancellation"); return nil }})
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop")
	}
}

func TestObservationTransitionsAndHeartbeat(t *testing.T) {
	now := time.Unix(1000, 0)
	var db, other observation
	if !db.due("ok", now) || !other.due("ok", now) {
		t.Fatal("initial samples missing")
	}
	if db.due("ok", now.Add(30*time.Second)) {
		t.Fatal("unchanged sample emitted")
	}
	if !db.due("deadline", now.Add(time.Minute)) {
		t.Fatal("outage missing")
	}
	if other.due("ok", now.Add(time.Minute)) {
		t.Fatal("cross-dependency state interference")
	}
	if db.due("deadline", now.Add(5*time.Minute)) {
		t.Fatal("heartbeat too early")
	}
	if !db.due("deadline", now.Add(6*time.Minute)) {
		t.Fatal("unhealthy heartbeat missing")
	}
	if !db.due("ok", now.Add(6*time.Minute+time.Second)) {
		t.Fatal("recovery missing")
	}
	if !other.due("ok", now.Add(5*time.Minute)) {
		t.Fatal("healthy heartbeat missing")
	}
	if !(*observation)(nil).due("ok", now) {
		t.Fatal("unthrottled worker changed")
	}
}

func TestAPSamplesContinueWhenEventsSuppressed(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	calls := 0
	var failure error
	var state observation
	check := Check{Name: "db", Ping: func(context.Context) error { calls++; return failure }}
	sample(context.Background(), check, &state)
	sample(context.Background(), check, &state)
	failure = errors.New("secret")
	sample(context.Background(), check, &state)
	sample(context.Background(), check, &state)
	state.emitted = time.Now().Add(-5 * time.Minute)
	sample(context.Background(), check, &state)
	failure = nil
	sample(context.Background(), check, &state)
	if calls != 6 || strings.Count(logs.String(), "dependency_health") != 4 || strings.Contains(logs.String(), "secret") {
		t.Fatalf("calls=%d events=%s", calls, logs.String())
	}
}
