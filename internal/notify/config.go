package notify

// Deprecated: this compatibility configuration remains until Task 9 moves the
// legacy runtime to the durable notification command. New code uses internal/config.

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port                       string
	InternalToken              string
	QueueDriver                string
	ServiceBusConnectionString string
	ServiceBusQueueName        string
	RedisAddr                  string
	RedisPassword              string
	EmailCooldown              time.Duration
	RecipientDailyLimit        int
	GlobalDailyLimit           int
	NotificationsDisabled      bool
	LogEmailBody               bool
	SMTPAddr                   string
	SMTPUsername               string
	SMTPPassword               string
	SMTPFrom                   string
}

func LoadConfig() Config {
	return Config{
		Port:                       env("PORT", "8081"),
		InternalToken:              os.Getenv("INTERNAL_API_TOKEN"),
		QueueDriver:                env("QUEUE_DRIVER", "memory"),
		ServiceBusConnectionString: os.Getenv("SERVICEBUS_CONNECTION_STRING"),
		ServiceBusQueueName:        env("SERVICEBUS_QUEUE_NAME", "notifications-email"),
		RedisAddr:                  env("REDIS_ADDR", "localhost:6379"),
		RedisPassword:              os.Getenv("REDIS_PASSWORD"),
		EmailCooldown:              durationEnv("EMAIL_COOLDOWN", 15*time.Minute),
		RecipientDailyLimit:        intEnv("EMAIL_RECIPIENT_DAILY_LIMIT", 5),
		GlobalDailyLimit:           intEnv("EMAIL_GLOBAL_DAILY_LIMIT", 1000),
		NotificationsDisabled:      boolEnv("NOTIFICATIONS_DISABLED", false),
		LogEmailBody:               boolEnv("LOG_EMAIL_BODY", false),
		SMTPAddr:                   os.Getenv("SMTP_ADDR"),
		SMTPUsername:               os.Getenv("SMTP_USERNAME"),
		SMTPPassword:               os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:                   os.Getenv("SMTP_FROM"),
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func intEnv(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return value
}

func boolEnv(key string, fallback bool) bool {
	value, err := strconv.ParseBool(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return value
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return value
}
