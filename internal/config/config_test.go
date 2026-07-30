package config

import (
	"encoding/base64"
	"reflect"
	"testing"
)

func TestLoadProductionRejectsMissingDatabaseURL(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("DATABASE_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := cfg.Validate("api"); err == nil {
		t.Fatal("Validate(api) error = nil, want missing DATABASE_URL rejected")
	}
}

func TestLoadProductionRejectsMemoryQueue(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("QUEUE_DRIVER", "memory")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := cfg.Validate("api"); err == nil {
		t.Fatal("Validate(api) error = nil, want memory queue rejected")
	}
}

func TestLoadProductionRejectsMissingProvider(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("SMTP_ADDR", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := cfg.Validate("worker"); err == nil {
		t.Fatal("Validate(worker) error = nil, want missing SMTP provider rejected")
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

func TestLoadDevelopmentRejectsMemoryQueue(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("DATABASE_URL", "postgres://notification:password@localhost:5432/notification")
	t.Setenv("NOTIFICATION_ALLOWED_CALLERS", "account-api")
	t.Setenv("NOTIFICATION_DATA_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("NOTIFICATION_HASH_KEY", "01234567890123456789012345678901")
	t.Setenv("QUEUE_DRIVER", "memory")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := cfg.Validate("api"); err == nil {
		t.Fatal("Validate(api) error = nil, want unsupported memory queue rejected")
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

func TestLoadTemplateDailyLimit(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TemplateDailyLimit != 1_000 {
		t.Fatalf("TemplateDailyLimit = %d, want 1000", cfg.TemplateDailyLimit)
	}

	t.Setenv("NOTIFICATION_TEMPLATE_DAILY_LIMIT", "250")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load(custom limit) error = %v", err)
	}
	if cfg.TemplateDailyLimit != 250 {
		t.Fatalf("TemplateDailyLimit = %d, want 250", cfg.TemplateDailyLimit)
	}
}

func TestLoadRejectsNonPositiveTemplateDailyLimit(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("NOTIFICATION_TEMPLATE_DAILY_LIMIT", "0")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want non-positive template daily limit rejected")
	}
}

func TestValidateModeRequirements(t *testing.T) {
	base := Config{
		Environment:                EnvironmentDevelopment,
		DatabaseURL:                "postgres://notification:password@localhost:5432/notification",
		AllowedCallers:             []string{"account-api"},
		DataEncryptionKey:          make([]byte, 32),
		HashKey:                    []byte("01234567890123456789012345678901"),
		QueueDriver:                "servicebus",
		ServiceBusConnectionString: "Endpoint=sb://localhost/;SharedAccessKeyName=local;SharedAccessKey=local",
		ServiceBusQueueName:        "notifications-email",
		SMTPAddr:                   "smtp.example.test:587",
		SMTPFrom:                   "noreply@alive.org.tw",
	}

	migrate := Config{
		Environment: EnvironmentDevelopment,
		DatabaseURL: base.DatabaseURL,
	}
	if err := migrate.Validate("migrate"); err != nil {
		t.Fatalf("Validate(migrate) error = %v", err)
	}

	api := base
	api.SMTPAddr = ""
	api.SMTPFrom = ""
	if err := api.Validate("api"); err != nil {
		t.Fatalf("Validate(api) error = %v", err)
	}

	worker := base
	worker.AllowedCallers = nil
	worker.HashKey = nil
	if err := worker.Validate("worker"); err != nil {
		t.Fatalf("Validate(worker) error = %v", err)
	}

	for _, test := range []struct {
		name string
		mode string
		edit func(*Config)
	}{
		{name: "all modes need database", mode: "migrate", edit: func(c *Config) { c.DatabaseURL = "" }},
		{name: "api needs encryption", mode: "api", edit: func(c *Config) { c.DataEncryptionKey = nil }},
		{name: "api needs hash", mode: "api", edit: func(c *Config) { c.HashKey = nil }},
		{name: "api needs callers", mode: "api", edit: func(c *Config) { c.AllowedCallers = nil }},
		{name: "api needs service bus", mode: "api", edit: func(c *Config) { c.QueueDriver = "memory" }},
		{name: "worker needs encryption", mode: "worker", edit: func(c *Config) { c.DataEncryptionKey = nil }},
		{name: "worker needs service bus", mode: "worker", edit: func(c *Config) { c.QueueDriver = "memory" }},
		{name: "worker needs smtp", mode: "worker", edit: func(c *Config) { c.SMTPAddr = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.edit(&cfg)
			if err := cfg.Validate(test.mode); err == nil {
				t.Fatalf("Validate(%s) error = nil", test.mode)
			}
		})
	}
}

func TestValidateProductionRestrictionsApplyToEveryMode(t *testing.T) {
	for _, mode := range []string{"api", "worker", "migrate"} {
		t.Run(mode+"/dev-header", func(t *testing.T) {
			cfg := validProductionModeConfig(mode)
			cfg.AllowDevCallerHeader = true
			if err := cfg.Validate(mode); err == nil {
				t.Fatalf("Validate(%s) accepted development caller header", mode)
			}
		})
		t.Run(mode+"/connection-string", func(t *testing.T) {
			cfg := validProductionModeConfig(mode)
			cfg.ServiceBusConnectionString = "Endpoint=sb://localhost/;SharedAccessKeyName=local;SharedAccessKey=local"
			if err := cfg.Validate(mode); err == nil {
				t.Fatalf("Validate(%s) accepted Service Bus connection string", mode)
			}
		})
	}
}

func validProductionModeConfig(mode string) Config {
	cfg := Config{
		Environment:         EnvironmentProduction,
		DatabaseURL:         "postgres://notification:password@localhost:5432/notification",
		DataEncryptionKey:   make([]byte, 32),
		QueueDriver:         "servicebus",
		ServiceBusNamespace: "notification.servicebus.windows.net",
		ServiceBusQueueName: "notifications-email",
		SMTPAddr:            "smtp.example.test:587",
		SMTPFrom:            "noreply@alive.org.tw",
	}
	if mode == "api" {
		cfg.AllowedCallers = []string{"account-api"}
		cfg.HashKey = []byte("01234567890123456789012345678901")
	}
	return cfg
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
		"ENVIRONMENT", "PORT", "DATABASE_URL", "NOTIFICATION_ALLOWED_CALLERS",
		"NOTIFICATION_ALLOW_DEV_CALLER_HEADER", "NOTIFICATION_DATA_ENCRYPTION_KEY", "NOTIFICATION_HASH_KEY",
		"QUEUE_DRIVER", "SERVICEBUS_NAMESPACE", "SERVICEBUS_QUEUE_NAME", "SERVICEBUS_CONNECTION_STRING",
		"SMTP_ADDR", "SMTP_USERNAME", "SMTP_PASSWORD", "SMTP_FROM", "NOTIFICATIONS_DISABLED",
		"NOTIFICATION_TEMPLATE_DAILY_LIMIT", "SHUTDOWN_TIMEOUT_SECONDS",
	} {
		t.Setenv(key, "")
	}
}
