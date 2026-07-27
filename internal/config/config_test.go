package config

import (
	"encoding/base64"
	"reflect"
	"testing"
)

func TestLoadProductionRejectsMissingDatabaseURL(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("DATABASE_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want missing DATABASE_URL rejected")
	}
}

func TestLoadProductionRejectsMemoryQueue(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("QUEUE_DRIVER", "memory")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want memory queue rejected in production")
	}
}

func TestLoadProductionRejectsMissingProvider(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("SMTP_ADDR", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want missing SMTP provider rejected in production")
	}
}

func TestLoadDevelopmentAllowsServiceBusConnectionString(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("QUEUE_DRIVER", "servicebus")
	t.Setenv("SERVICEBUS_CONNECTION_STRING", "Endpoint=sb://localhost/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=local")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ServiceBusConnectionString == "" {
		t.Fatal("Load() did not retain the development Service Bus connection string")
	}
}

func TestLoadParsesAllowedCallers(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("NOTIFICATION_ALLOWED_CALLERS", " account-api, hhc-web-api ,account-api ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"account-api", "hhc-web-api"}
	if !reflect.DeepEqual(cfg.AllowedCallers, want) {
		t.Fatalf("AllowedCallers = %#v, want %#v", cfg.AllowedCallers, want)
	}
}

func setProductionEnv(t *testing.T) {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("ENVIRONMENT", "production")
	t.Setenv("DATABASE_URL", "postgres://notification:password@localhost:5432/notification")
	t.Setenv("NOTIFICATION_ALLOWED_CALLERS", "account-api,hhc-web-api")
	t.Setenv("NOTIFICATION_DATA_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("NOTIFICATION_HASH_KEY", "01234567890123456789012345678901")
	t.Setenv("QUEUE_DRIVER", "servicebus")
	t.Setenv("SERVICEBUS_NAMESPACE", "notification.servicebus.windows.net")
	t.Setenv("SMTP_ADDR", "smtp.example.test:587")
	t.Setenv("SMTP_FROM", "noreply@example.test")
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"ENVIRONMENT", "NOTIFICATION_MODE", "PORT", "DATABASE_URL", "NOTIFICATION_ALLOWED_CALLERS",
		"NOTIFICATION_ALLOW_DEV_CALLER_HEADER", "NOTIFICATION_DATA_ENCRYPTION_KEY", "NOTIFICATION_HASH_KEY",
		"QUEUE_DRIVER", "SERVICEBUS_NAMESPACE", "SERVICEBUS_QUEUE_NAME", "SERVICEBUS_CONNECTION_STRING",
		"SMTP_ADDR", "SMTP_USERNAME", "SMTP_PASSWORD", "SMTP_FROM", "NOTIFICATIONS_DISABLED",
		"SHUTDOWN_TIMEOUT_SECONDS",
	} {
		t.Setenv(key, "")
	}
}
