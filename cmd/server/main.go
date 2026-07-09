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
	"github.com/redis/go-redis/v9"
)

func main() {
	cfg := notify.LoadConfig()
	logger := log.New(os.Stdout, "", log.LstdFlags)

	queue, err := newQueue(cfg)
	if err != nil {
		logger.Fatalf("queue setup failed: %v", err)
	}

	limiter := newLimiter(cfg)
	sender := newSender(cfg, logger)
	service := notify.NewService(limiter, queue)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := notify.RunWorker(ctx, queue, sender, logger); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("worker stopped: %v", err)
		}
	}()

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           notify.NewHandler(service, cfg.InternalToken),
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

func newLimiter(cfg notify.Config) notify.Limiter {
	if cfg.NotificationsDisabled {
		return notify.DisabledLimiter{}
	}
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	})
	return notify.NewRedisLimiter(client, cfg.EmailCooldown, cfg.RecipientDailyLimit, cfg.GlobalDailyLimit)
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
