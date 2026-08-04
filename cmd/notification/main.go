package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/config"
	"github.com/HallelujahHomeChurch/notification-api/internal/database"
	"github.com/HallelujahHomeChurch/notification-api/internal/httpapi"
	"github.com/HallelujahHomeChurch/notification-api/internal/migrations"
	"github.com/HallelujahHomeChurch/notification-api/internal/outbox"
	"github.com/HallelujahHomeChurch/notification-api/internal/providers"
	"github.com/HallelujahHomeChurch/notification-api/internal/queue"
	"github.com/HallelujahHomeChurch/notification-api/internal/retention"
	"github.com/HallelujahHomeChurch/notification-api/internal/service"
	"github.com/HallelujahHomeChurch/notification-api/internal/store"
	"github.com/HallelujahHomeChurch/notification-api/internal/worker"
)

const startupTimeout = 10 * time.Second

type process func(context.Context) error
type closeFunc func(context.Context) error

type apiComponents struct {
	http      process
	outbox    process
	retention process
	close     closeFunc
}

type workerComponents struct {
	http     process
	consumer process
	close    closeFunc
}

type app struct {
	loadConfig  func() (config.Config, error)
	buildAPI    func(context.Context, config.Config) (apiComponents, error)
	buildWorker func(context.Context, config.Config) (workerComponents, error)
	migrate     func(context.Context, config.Config) error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application := productionApp()
	if err := application.run(ctx, os.Args[1:]); err != nil {
		log.Printf("notification-api stopped: %v", err)
		os.Exit(1)
	}
}

func productionApp() app {
	return app{
		loadConfig:  config.Load,
		buildAPI:    buildAPI,
		buildWorker: buildWorker,
		migrate:     runMigrations,
	}
}

func (a app) run(ctx context.Context, args []string) error {
	if len(args) != 1 || (args[0] != "api" && args[0] != "worker" && args[0] != "migrate") {
		return errors.New("usage: notification-api api|worker|migrate")
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(args[0]); err != nil {
		return fmt.Errorf("validate %s config: %w", args[0], err)
	}
	switch args[0] {
	case "api":
		components, err := a.buildAPI(ctx, cfg)
		if err != nil {
			return err
		}
		return runProcesses(ctx, cfg.ShutdownTimeout, components.close,
			components.http, components.outbox, components.retention)
	case "worker":
		components, err := a.buildWorker(ctx, cfg)
		if err != nil {
			return err
		}
		return runProcesses(ctx, cfg.ShutdownTimeout, components.close,
			components.http, components.consumer)
	default:
		return a.migrate(ctx, cfg)
	}
}

func runProcesses(ctx context.Context, timeout time.Duration, close closeFunc, processes ...process) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithCancel(ctx)
	results := make(chan error, len(processes))
	for _, run := range processes {
		go func() { results <- run(runCtx) }()
	}

	var (
		runErr    error
		completed int
	)
	select {
	case <-ctx.Done():
	case err := <-results:
		completed++
		if err == nil {
			runErr = errors.New("required process stopped unexpectedly")
		} else if !errors.Is(err, context.Canceled) {
			runErr = err
		}
	}
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), timeout)
	defer shutdownCancel()
	for completed < len(processes) {
		select {
		case err := <-results:
			completed++
			if err != nil && !errors.Is(err, context.Canceled) {
				runErr = errors.Join(runErr, err)
			}
		case <-shutdownCtx.Done():
			return errors.Join(runErr, shutdownCtx.Err())
		}
	}
	if close == nil {
		return runErr
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- close(shutdownCtx) }()
	select {
	case err := <-closeResult:
		return errors.Join(runErr, err)
	case <-shutdownCtx.Done():
		return errors.Join(runErr, shutdownCtx.Err())
	}
}

func buildAPI(ctx context.Context, cfg config.Config) (apiComponents, error) {
	db, err := openDatabase(ctx, cfg)
	if err != nil {
		return apiComponents{}, err
	}
	if err := store.ValidateKeyReferences(ctx, db, nil, cfg.HashKeys); err != nil {
		_ = db.Close()
		return apiComponents{}, fmt.Errorf("validate notification hash keys: %w", err)
	}
	publisher, err := queue.NewServiceBus(serviceBusConfig(cfg))
	if err != nil {
		_ = db.Close()
		return apiComponents{}, fmt.Errorf("create publisher: %w", err)
	}
	repository := store.NewWithHashKeys(db, cfg.HashKeys)
	notificationService := service.New(repository, service.Config{
		ActiveEncryptionKeyID: cfg.ActiveEncryptionKeyID,
		EncryptionKeys:        cfg.EncryptionKeys,
		ActiveHashKeyID:       cfg.ActiveHashKeyID,
		HashKeys:              cfg.HashKeys,
		NotificationsDisabled: cfg.NotificationsDisabled,
		RateLimits: []store.RateLimit{
			{Window: 15 * time.Minute, Maximum: 1},
			{Window: 24 * time.Hour, Maximum: 5},
			{Window: 24 * time.Hour, Maximum: cfg.TemplateDailyLimit, TemplateWide: true},
		},
	})
	handler := httpapi.New(
		notificationService,
		db,
		cfg.AllowedCallers,
		cfg.AllowDevCallerHeader,
	)
	dispatcher := outbox.New(db, publisher)
	retentionWorker := retention.New(db)

	return apiComponents{
		http:      httpProcess(cfg.Port, handler, cfg.ShutdownTimeout),
		outbox:    dispatcher.Run,
		retention: retentionWorker.Run,
		close: func(ctx context.Context) error {
			return errors.Join(publisher.Close(ctx), db.Close())
		},
	}, nil
}

func buildWorker(ctx context.Context, cfg config.Config) (workerComponents, error) {
	smtpConfig := providers.SMTPConfig{
		Addr:     cfg.SMTPAddr,
		Username: cfg.SMTPUsername,
		Password: cfg.SMTPPassword,
		From:     cfg.SMTPFrom,
		FromName: cfg.SMTPFromName,
		Logger:   log.New(os.Stdout, "", log.LstdFlags),
	}
	if err := providers.ValidateSMTPConfig(smtpConfig); err != nil {
		return workerComponents{}, fmt.Errorf("validate SMTP config: %w", err)
	}
	db, err := openDatabase(ctx, cfg)
	if err != nil {
		return workerComponents{}, err
	}
	if err := store.ValidateKeyReferences(ctx, db, cfg.EncryptionKeys, nil); err != nil {
		_ = db.Close()
		return workerComponents{}, fmt.Errorf("validate notification encryption keys: %w", err)
	}
	consumer, err := queue.NewServiceBusConsumer(serviceBusConfig(cfg))
	if err != nil {
		_ = db.Close()
		return workerComponents{}, fmt.Errorf("create consumer: %w", err)
	}
	provider := providers.NewSMTP(smtpConfig)
	deliveryWorker := worker.NewWithKeyring(db, provider, cfg.EncryptionKeys)

	return workerComponents{
		http: httpProcess(cfg.Port, workerHealthHandler(db), cfg.ShutdownTimeout),
		consumer: func(ctx context.Context) error {
			return consumer.Run(ctx, deliveryWorker.Process)
		},
		close: func(ctx context.Context) error {
			return errors.Join(consumer.Close(ctx), db.Close())
		},
	}, nil
}

func runMigrations(ctx context.Context, cfg config.Config) error {
	db, err := database.Open(cfg)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if err := migrations.Run(ctx, db); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

func openDatabase(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	db, err := database.Open(cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

func serviceBusConfig(cfg config.Config) queue.ServiceBusConfig {
	return queue.ServiceBusConfig{
		NamespaceFQDN:    cfg.ServiceBusNamespace,
		ConnectionString: cfg.ServiceBusConnectionString,
		QueueName:        cfg.ServiceBusQueueName,
	}
}

func httpProcess(port string, handler http.Handler, timeout time.Duration) process {
	return func(ctx context.Context) error {
		server := &http.Server{
			Addr:              ":" + port,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		serverResult := make(chan error, 1)
		go func() { serverResult <- server.ListenAndServe() }()

		select {
		case err := <-serverResult:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			return shutdownHTTPServer(ctx, server, serverResult, timeout)
		}
	}
}

func shutdownHTTPServer(
	ctx context.Context,
	server *http.Server,
	serverResult <-chan error,
	timeout time.Duration,
) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	var closeErr error
	if shutdownErr != nil {
		closeErr = normalizeHTTPServerError(server.Close())
	}

	select {
	case serverErr := <-serverResult:
		if err := errors.Join(
			shutdownErr,
			closeErr,
			normalizeHTTPServerError(serverErr),
		); err != nil {
			return err
		}
		return ctx.Err()
	case <-shutdownCtx.Done():
		closeErr = errors.Join(closeErr, normalizeHTTPServerError(server.Close()))
		var serverErr error
		select {
		case result := <-serverResult:
			serverErr = normalizeHTTPServerError(result)
		default:
		}
		return errors.Join(shutdownCtx.Err(), shutdownErr, closeErr, serverErr)
	}
}

func normalizeHTTPServerError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func workerHealthHandler(db *sql.DB) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusOK, "healthy")
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			writeStatus(w, http.StatusServiceUnavailable, "not_ready")
			return
		}
		writeStatus(w, http.StatusOK, "ready")
	})
	return mux
}

func writeStatus(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": value})
}
