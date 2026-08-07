package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/config"
)

func TestRunRejectsInvalidModeBeforeLoadingConfig(t *testing.T) {
	loaded := false
	application := app{
		loadConfig: func() (config.Config, error) {
			loaded = true
			return config.Config{}, nil
		},
	}

	for _, args := range [][]string{nil, {"unknown"}, {"api", "worker"}} {
		if err := application.run(context.Background(), args); err == nil {
			t.Fatalf("run(%v) error = nil", args)
		}
	}
	if loaded {
		t.Fatal("invalid mode loaded configuration")
	}
}

func TestRunAPIModeStartsHTTPOutboxAndRetention(t *testing.T) {
	started := make(chan string, 3)
	closed := make(chan struct{})
	application := app{
		loadConfig: testConfig,
		buildAPI: func(context.Context, config.Config) (apiComponents, error) {
			return apiComponents{
				http:      blockingProcess("http", started),
				outbox:    blockingProcess("outbox", started),
				retention: blockingProcess("retention", started),
				close: func(context.Context) error {
					close(closed)
					return nil
				},
			}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- application.run(ctx, []string{"api"}) }()

	assertStarted(t, started, "http", "outbox", "retention")
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("run(api) error = %v", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("api resources were not closed")
	}
}

func TestRunWorkerModeStartsConsumerAndReadinessHTTP(t *testing.T) {
	started := make(chan string, 2)
	application := app{
		loadConfig: testConfig,
		buildWorker: func(context.Context, config.Config) (workerComponents, error) {
			return workerComponents{
				http:     blockingProcess("http", started),
				consumer: blockingProcess("consumer", started),
			}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- application.run(ctx, []string{"worker"}) }()

	assertStarted(t, started, "http", "consumer")
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("run(worker) error = %v", err)
	}
}

func TestRunMigrateRunsOnceAndExits(t *testing.T) {
	calls := 0
	application := app{
		loadConfig: testConfig,
		migrate: func(context.Context, config.Config) error {
			calls++
			return nil
		},
	}

	if err := application.run(context.Background(), []string{"migrate"}); err != nil {
		t.Fatalf("run(migrate) error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("migration calls = %d, want 1", calls)
	}
}

func TestRunValidatesSelectedModeBeforeBuilding(t *testing.T) {
	built := false
	application := app{
		loadConfig: func() (config.Config, error) {
			return config.Config{
				Environment: config.EnvironmentDevelopment,
				DatabaseURL: "postgres://notification:password@localhost:5432/notification",
			}, nil
		},
		buildAPI: func(context.Context, config.Config) (apiComponents, error) {
			built = true
			return apiComponents{}, errors.New("runtime built")
		},
	}

	if err := application.run(context.Background(), []string{"api"}); err == nil {
		t.Fatal("run(api) error = nil with migrate-only config")
	}
	if built {
		t.Fatal("run(api) built runtime before validating selected mode")
	}
}

func TestRequiredProcessFailureCancelsSiblings(t *testing.T) {
	processErr := errors.New("outbox failed")
	siblingCanceled := make(chan struct{})
	application := app{
		loadConfig: testConfig,
		buildAPI: func(context.Context, config.Config) (apiComponents, error) {
			return apiComponents{
				http: func(ctx context.Context) error {
					<-ctx.Done()
					close(siblingCanceled)
					return ctx.Err()
				},
				outbox: func(context.Context) error {
					return processErr
				},
				retention: func(ctx context.Context) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}, nil
		},
	}

	err := application.run(context.Background(), []string{"api"})
	if !errors.Is(err, processErr) {
		t.Fatalf("run(api) error = %v, want %v", err, processErr)
	}
	select {
	case <-siblingCanceled:
	case <-time.After(time.Second):
		t.Fatal("required process failure did not cancel sibling")
	}
}

func TestShutdownTimeoutBoundsClose(t *testing.T) {
	application := app{
		loadConfig: func() (config.Config, error) {
			cfg, _ := testConfig()
			cfg.ShutdownTimeout = 20 * time.Millisecond
			return cfg, nil
		},
		buildWorker: func(context.Context, config.Config) (workerComponents, error) {
			return workerComponents{
				http: func(ctx context.Context) error {
					<-ctx.Done()
					return ctx.Err()
				},
				consumer: func(ctx context.Context) error {
					<-ctx.Done()
					return ctx.Err()
				},
				close: func(ctx context.Context) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err := application.run(ctx, []string{"worker"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run(worker) error = %v, want shutdown deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown elapsed = %s, want bounded", elapsed)
	}
}

func TestShutdownHTTPServerDoesNotWaitPastDeadlineForServerResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	serverResult := make(chan error)

	started := time.Now()
	err := shutdownHTTPServer(ctx, &http.Server{}, serverResult, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdownHTTPServer() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdownHTTPServer() elapsed = %s, want bounded", elapsed)
	}
}

func TestBuildWorkerRejectsInvalidSMTPBeforeDatabase(t *testing.T) {
	cfg := config.Config{
		Environment:                config.EnvironmentDevelopment,
		DatabaseURL:                "postgres://notification:password@127.0.0.1:1/notification",
		DataEncryptionKey:          make([]byte, 32),
		HashKey:                    []byte("01234567890123456789012345678901"),
		QueueDriver:                "servicebus",
		ServiceBusConnectionString: "Endpoint=sb://localhost/;SharedAccessKeyName=local;SharedAccessKey=local",
		ServiceBusQueueName:        "notifications-email",
		SMTPAddr:                   "smtp.example.test",
		SMTPFrom:                   "noreply@alive.org.tw",
		VAPIDPublicKey:             "BGsX0fLhLEJH-Lzm5WOkQPJ3A32BLeszoPShOUXYmMKWT-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU",
		VAPIDPrivateKey:            "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE",
		VAPIDSubject:               "mailto:support@alive.org.tw",
	}

	_, err := buildWorker(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "SMTP") {
		t.Fatalf("buildWorker() error = %v, want SMTP config error before database", err)
	}
}

func TestBuildWorkerRejectsInvalidWebPushBeforeDatabase(t *testing.T) {
	cfg, _ := testConfig()
	cfg.DatabaseURL = "postgres://notification:password@127.0.0.1:1/notification"
	cfg.VAPIDSubject = "not-a-subject"

	_, err := buildWorker(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "Web Push") {
		t.Fatalf("buildWorker() error = %v, want Web Push config error before database", err)
	}
}

func testConfig() (config.Config, error) {
	return config.Config{
		Environment:                config.EnvironmentDevelopment,
		ShutdownTimeout:            time.Second,
		DatabaseURL:                "postgres://notification:password@localhost:5432/notification",
		AllowedCallers:             []string{"account-api"},
		DataEncryptionKey:          make([]byte, 32),
		HashKey:                    []byte("01234567890123456789012345678901"),
		QueueDriver:                "servicebus",
		ServiceBusConnectionString: "Endpoint=sb://localhost/;SharedAccessKeyName=local;SharedAccessKey=local",
		ServiceBusQueueName:        "notifications-email",
		SMTPAddr:                   "smtp.example.test:587",
		SMTPFrom:                   "noreply@alive.org.tw",
		VAPIDPublicKey:             "BGsX0fLhLEJH-Lzm5WOkQPJ3A32BLeszoPShOUXYmMKWT-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU",
		VAPIDPrivateKey:            "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE",
		VAPIDSubject:               "mailto:support@alive.org.tw",
	}, nil
}

func blockingProcess(name string, started chan<- string) process {
	return func(ctx context.Context) error {
		started <- name
		<-ctx.Done()
		return ctx.Err()
	}
}

func assertStarted(t *testing.T, started <-chan string, want ...string) {
	t.Helper()
	got := make(map[string]bool, len(want))
	for range want {
		select {
		case name := <-started:
			got[name] = true
		case <-time.After(time.Second):
			t.Fatalf("started = %v, want %v", got, want)
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("started = %v, missing %q", got, name)
		}
	}
}
