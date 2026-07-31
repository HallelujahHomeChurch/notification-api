package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadParsesVersionedKeyrings(t *testing.T) {
	clearConfigEnv(t)
	encryptionV1 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	encryptionV2 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID", "enc-v2")
	t.Setenv("NOTIFICATION_ENCRYPTION_KEYS_JSON", fmt.Sprintf(
		`{"legacy-v1":%q,"enc-v2":%q}`, encryptionV1, encryptionV2,
	))
	t.Setenv("NOTIFICATION_ACTIVE_HASH_KEY_ID", "hash-v2")
	t.Setenv("NOTIFICATION_HASH_KEYS_JSON",
		`{"legacy-v1":"01234567890123456789012345678901","hash-v2":"abcdefghijklmnopqrstuvwxyz012345"}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ActiveEncryptionKeyID != "enc-v2" || len(cfg.EncryptionKeys) != 2 {
		t.Fatalf("encryption keyring = %q %#v", cfg.ActiveEncryptionKeyID, cfg.EncryptionKeys)
	}
	if cfg.ActiveHashKeyID != "hash-v2" || len(cfg.HashKeys) != 2 {
		t.Fatalf("hash keyring = %q %#v", cfg.ActiveHashKeyID, cfg.HashKeys)
	}
	if !reflect.DeepEqual(cfg.DataEncryptionKey, cfg.EncryptionKeys["enc-v2"]) {
		t.Fatal("DataEncryptionKey does not preserve the active key for current callers")
	}
	if !reflect.DeepEqual(cfg.HashKey, cfg.HashKeys["hash-v2"]) {
		t.Fatal("HashKey does not preserve the active key for current callers")
	}
}

func TestLoadMapsLegacyKeysToLegacyV1(t *testing.T) {
	clearConfigEnv(t)
	encryptionKey := make([]byte, 32)
	hashKey := "01234567890123456789012345678901"
	t.Setenv("NOTIFICATION_DATA_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(encryptionKey))
	t.Setenv("NOTIFICATION_HASH_KEY", hashKey)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ActiveEncryptionKeyID != "legacy-v1" ||
		!reflect.DeepEqual(cfg.EncryptionKeys["legacy-v1"], encryptionKey) {
		t.Fatalf("legacy encryption keyring = %q %#v", cfg.ActiveEncryptionKeyID, cfg.EncryptionKeys)
	}
	if cfg.ActiveHashKeyID != "legacy-v1" ||
		string(cfg.HashKeys["legacy-v1"]) != hashKey {
		t.Fatalf("legacy hash keyring = %q %#v", cfg.ActiveHashKeyID, cfg.HashKeys)
	}
}

func TestLoadRequiresLegacyAndVersionedKeysToMatch(t *testing.T) {
	clearConfigEnv(t)
	legacyEncryption := base64.StdEncoding.EncodeToString(make([]byte, 32))
	differentEncryption := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	t.Setenv("NOTIFICATION_DATA_ENCRYPTION_KEY", legacyEncryption)
	t.Setenv("NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID", "legacy-v1")
	t.Setenv("NOTIFICATION_ENCRYPTION_KEYS_JSON", fmt.Sprintf(`{"legacy-v1":%q}`, differentEncryption))
	t.Setenv("NOTIFICATION_HASH_KEY", "01234567890123456789012345678901")
	t.Setenv("NOTIFICATION_ACTIVE_HASH_KEY_ID", "legacy-v1")
	t.Setenv("NOTIFICATION_HASH_KEYS_JSON", `{"legacy-v1":"abcdefghijklmnopqrstuvwxyz012345"}`)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("Load() error = %v, want legacy/keyring mismatch", err)
	}

	t.Setenv("NOTIFICATION_ENCRYPTION_KEYS_JSON", fmt.Sprintf(`{"legacy-v1":%q}`, legacyEncryption))
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "NOTIFICATION_HASH_KEY must match") {
		t.Fatalf("Load() error = %v, want hash legacy/keyring mismatch", err)
	}
}

func TestLoadRejectsInvalidKeyrings(t *testing.T) {
	validEncryption := base64.StdEncoding.EncodeToString(make([]byte, 32))
	validHash := "01234567890123456789012345678901"
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "duplicate encryption id",
			env: map[string]string{
				"NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID": "v1",
				"NOTIFICATION_ENCRYPTION_KEYS_JSON":     fmt.Sprintf(`{"v1":%q,"v1":%q}`, validEncryption, validEncryption),
			},
			want: "duplicate key ID",
		},
		{
			name: "empty encryption id",
			env: map[string]string{
				"NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID": "v1",
				"NOTIFICATION_ENCRYPTION_KEYS_JSON":     fmt.Sprintf(`{"":%q}`, validEncryption),
			},
			want: "empty key ID",
		},
		{
			name: "missing active encryption id",
			env: map[string]string{
				"NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID": "v2",
				"NOTIFICATION_ENCRYPTION_KEYS_JSON":     fmt.Sprintf(`{"v1":%q}`, validEncryption),
			},
			want: "active key",
		},
		{
			name: "invalid encryption base64",
			env: map[string]string{
				"NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID": "v1",
				"NOTIFICATION_ENCRYPTION_KEYS_JSON":     `{"v1":"not-base64"}`,
			},
			want: "base64",
		},
		{
			name: "wrong encryption length",
			env: map[string]string{
				"NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID": "v1",
				"NOTIFICATION_ENCRYPTION_KEYS_JSON":     fmt.Sprintf(`{"v1":%q}`, base64.StdEncoding.EncodeToString(make([]byte, 31))),
			},
			want: "32 bytes",
		},
		{
			name: "duplicate hash id",
			env: map[string]string{
				"NOTIFICATION_ACTIVE_HASH_KEY_ID": "v1",
				"NOTIFICATION_HASH_KEYS_JSON":     fmt.Sprintf(`{"v1":%q,"v1":%q}`, validHash, validHash),
			},
			want: "duplicate key ID",
		},
		{
			name: "short hash key",
			env: map[string]string{
				"NOTIFICATION_ACTIVE_HASH_KEY_ID": "v1",
				"NOTIFICATION_HASH_KEYS_JSON":     `{"v1":"short"}`,
			},
			want: "at least 32 bytes",
		},
		{
			name: "incomplete versioned config",
			env: map[string]string{
				"NOTIFICATION_ACTIVE_HASH_KEY_ID": "v1",
			},
			want: "must be set together",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnv(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

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

func TestLoadDatabasePoolDefaultsAndOverrides(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DBMaxOpenConns != 5 {
		t.Fatalf("DBMaxOpenConns = %d, want 5", cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != 2 {
		t.Fatalf("DBMaxIdleConns = %d, want 2", cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLifetime != 30*time.Minute {
		t.Fatalf("DBConnMaxLifetime = %s, want 30m", cfg.DBConnMaxLifetime)
	}

	t.Setenv("DB_MAX_OPEN_CONNS", "8")
	t.Setenv("DB_MAX_IDLE_CONNS", "3")
	t.Setenv("DB_CONN_MAX_LIFETIME", "45m")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load(overrides) error = %v", err)
	}
	if cfg.DBMaxOpenConns != 8 || cfg.DBMaxIdleConns != 3 || cfg.DBConnMaxLifetime != 45*time.Minute {
		t.Fatalf("database pool config = %d/%d/%s, want 8/3/45m",
			cfg.DBMaxOpenConns, cfg.DBMaxIdleConns, cfg.DBConnMaxLifetime)
	}
}

func TestLoadRejectsInvalidDatabasePool(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "nonpositive open", env: map[string]string{"DB_MAX_OPEN_CONNS": "0"}, want: "DB_MAX_OPEN_CONNS"},
		{name: "nonpositive idle", env: map[string]string{"DB_MAX_IDLE_CONNS": "0"}, want: "DB_MAX_IDLE_CONNS"},
		{name: "idle exceeds open", env: map[string]string{
			"DB_MAX_OPEN_CONNS": "2",
			"DB_MAX_IDLE_CONNS": "3",
		}, want: "must not exceed"},
		{name: "nonpositive lifetime", env: map[string]string{
			"DB_CONN_MAX_LIFETIME": "0s",
		}, want: "DB_CONN_MAX_LIFETIME"},
		{name: "invalid lifetime", env: map[string]string{
			"DB_CONN_MAX_LIFETIME": "later",
		}, want: "DB_CONN_MAX_LIFETIME"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnv(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want containing %q", err, test.want)
			}
		})
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
		"NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID", "NOTIFICATION_ENCRYPTION_KEYS_JSON",
		"NOTIFICATION_ACTIVE_HASH_KEY_ID", "NOTIFICATION_HASH_KEYS_JSON",
		"QUEUE_DRIVER", "SERVICEBUS_NAMESPACE", "SERVICEBUS_QUEUE_NAME", "SERVICEBUS_CONNECTION_STRING",
		"SMTP_ADDR", "SMTP_USERNAME", "SMTP_PASSWORD", "SMTP_FROM", "NOTIFICATIONS_DISABLED",
		"NOTIFICATION_TEMPLATE_DAILY_LIMIT", "SHUTDOWN_TIMEOUT_SECONDS",
		"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
	} {
		t.Setenv(key, "")
	}
}
