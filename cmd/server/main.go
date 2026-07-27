package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/notify"
)

func main() {
	cfg := notify.LoadConfig()
	logger := log.New(os.Stdout, "", log.LstdFlags)

	queue, err := newQueue(cfg)
	if err != nil {
		logger.Fatalf("queue setup failed: %v", err)
	}

	sender := newSender(cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := notify.RunWorker(ctx, queue, sender, logger); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("worker stopped: %v", err)
		}
	}()

	legacyMux := http.NewServeMux()
	legacyMux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           legacyMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Printf("notification-api listening on :%s queue=%s", cfg.Port, cfg.QueueDriver)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("server stopped: %v", err)
	}
}

func newQueue(cfg notify.Config) (interface {
	notify.Queue
	notify.Consumer
}, error) {
	switch cfg.QueueDriver {
	case "memory":
		return notify.NewMemoryQueue(100), nil
	case "servicebus":
		return notify.NewServiceBusQueue(cfg.ServiceBusConnectionString, cfg.ServiceBusQueueName)
	default:
		return nil, errors.New("QUEUE_DRIVER must be memory or servicebus")
	}
}

func newSender(cfg notify.Config, logger *log.Logger) notify.Sender {
	if cfg.SMTPAddr == "" {
		return notify.LogSender{Logger: logger, LogBody: cfg.LogEmailBody}
	}
	return notify.SMTPSender{
		Addr:     cfg.SMTPAddr,
		Username: cfg.SMTPUsername,
		Password: cfg.SMTPPassword,
		From:     cfg.SMTPFrom,
	}
}
