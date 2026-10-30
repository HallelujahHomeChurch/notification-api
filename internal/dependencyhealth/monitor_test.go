package dependencyhealth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
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
		{nil, "ok"}, {context.DeadlineExceeded, "deadline"}, {context.Canceled, "cancelled"}, {errNotConfigured, "not_configured"}, {errors.New("secret postgres://token@host/query"), "unavailable"},
	} {
		logs.Reset()
		sample(context.Background(), Check{Name: "db", Ping: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 2*time.Second {
				t.Fatal("missing bound")
			}
			return tc.err
		}})
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

func TestDaprProbeDoesNotFollowRedirectOrLeakToken(t *testing.T) {
	called := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("dapr-api-token") != "test-token" {
			t.Error("token missing")
		}
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	if err := Dapr(port, "test-token").Ping(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
	if called {
		t.Fatal("redirect followed")
	}
	if !errors.Is(Dapr("", "").Ping(context.Background()), errNotConfigured) {
		t.Fatal("missing configuration")
	}
}
