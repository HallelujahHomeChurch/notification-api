package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvironmentDevelopment = "development"
	EnvironmentProduction  = "production"
)

type Config struct {
	Environment                string
	Mode                       string
	Port                       string
	DatabaseURL                string
	AllowedCallers             []string
	AllowDevCallerHeader       bool
	DataEncryptionKey          []byte
	HashKey                    []byte
	QueueDriver                string
	ServiceBusNamespace        string
	ServiceBusQueueName        string
	ServiceBusConnectionString string
	SMTPAddr                   string
	SMTPUsername               string
	SMTPPassword               string
	SMTPFrom                   string
	NotificationsDisabled      bool
	ShutdownTimeout            time.Duration
}

func Load() (Config, error) {
	allowDevCallerHeader, err := boolEnv("NOTIFICATION_ALLOW_DEV_CALLER_HEADER", false)
	if err != nil {
		return Config{}, err
	}
	notificationsDisabled, err := boolEnv("NOTIFICATIONS_DISABLED", false)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeoutSeconds, err := intEnv("SHUTDOWN_TIMEOUT_SECONDS", 30)
	if err != nil {
		return Config{}, err
	}
	if shutdownTimeoutSeconds <= 0 {
		return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT_SECONDS must be positive")
	}

	cfg := Config{
		Environment:                env("ENVIRONMENT", EnvironmentDevelopment),
		Mode:                       env("NOTIFICATION_MODE", "api"),
		Port:                       env("PORT", "8081"),
		DatabaseURL:                os.Getenv("DATABASE_URL"),
		AllowedCallers:             parseCallers(os.Getenv("NOTIFICATION_ALLOWED_CALLERS")),
		AllowDevCallerHeader:       allowDevCallerHeader,
		HashKey:                    []byte(os.Getenv("NOTIFICATION_HASH_KEY")),
		QueueDriver:                env("QUEUE_DRIVER", "memory"),
		ServiceBusNamespace:        os.Getenv("SERVICEBUS_NAMESPACE"),
		ServiceBusQueueName:        env("SERVICEBUS_QUEUE_NAME", "notifications-email"),
		ServiceBusConnectionString: os.Getenv("SERVICEBUS_CONNECTION_STRING"),
		SMTPAddr:                   os.Getenv("SMTP_ADDR"),
		SMTPUsername:               os.Getenv("SMTP_USERNAME"),
		SMTPPassword:               os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:                   os.Getenv("SMTP_FROM"),
		NotificationsDisabled:      notificationsDisabled,
		ShutdownTimeout:            time.Duration(shutdownTimeoutSeconds) * time.Second,
	}

	if value := os.Getenv("NOTIFICATION_DATA_ENCRYPTION_KEY"); value != "" {
		cfg.DataEncryptionKey, err = base64.StdEncoding.DecodeString(value)
		if err != nil {
			return Config{}, fmt.Errorf("NOTIFICATION_DATA_ENCRYPTION_KEY must be base64: %w", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.Environment != EnvironmentDevelopment && c.Environment != EnvironmentProduction {
		return fmt.Errorf("ENVIRONMENT must be development or production")
	}
	if c.Mode != "api" && c.Mode != "worker" && c.Mode != "migrate" {
		return fmt.Errorf("NOTIFICATION_MODE must be api, worker, or migrate")
	}
	if c.QueueDriver != "memory" && c.QueueDriver != "servicebus" {
		return fmt.Errorf("QUEUE_DRIVER must be memory or servicebus")
	}
	if c.Environment != EnvironmentProduction {
		return nil
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required in production")
	}
	if len(c.DataEncryptionKey) != 32 {
		return fmt.Errorf("NOTIFICATION_DATA_ENCRYPTION_KEY must decode to 32 bytes in production")
	}
	if len(c.HashKey) < 32 {
		return fmt.Errorf("NOTIFICATION_HASH_KEY must contain at least 32 bytes in production")
	}
	if len(c.AllowedCallers) == 0 {
		return fmt.Errorf("NOTIFICATION_ALLOWED_CALLERS is required in production")
	}
	if c.QueueDriver != "servicebus" {
		return fmt.Errorf("QUEUE_DRIVER must be servicebus in production")
	}
	if c.ServiceBusNamespace == "" {
		return fmt.Errorf("SERVICEBUS_NAMESPACE is required in production")
	}
	if c.ServiceBusConnectionString != "" {
		return fmt.Errorf("SERVICEBUS_CONNECTION_STRING is development-only")
	}
	if c.AllowDevCallerHeader {
		return fmt.Errorf("NOTIFICATION_ALLOW_DEV_CALLER_HEADER must be false in production")
	}
	if c.SMTPAddr == "" || c.SMTPFrom == "" {
		return fmt.Errorf("SMTP_ADDR and SMTP_FROM are required in production")
	}
	return nil
}

func parseCallers(value string) []string {
	seen := make(map[string]struct{})
	var callers []string
	for _, caller := range strings.Split(value, ",") {
		caller = strings.TrimSpace(caller)
		if caller == "" {
			continue
		}
		if _, ok := seen[caller]; ok {
			continue
		}
		seen[caller] = struct{}{}
		callers = append(callers, caller)
	}
	return callers
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func boolEnv(key string, fallback bool) (bool, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be boolean: %w", key, err)
	}
	return parsed, nil
}

func intEnv(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return parsed, nil
}
