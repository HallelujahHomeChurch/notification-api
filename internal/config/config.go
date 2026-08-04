package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvironmentDevelopment   = "development"
	EnvironmentProduction    = "production"
	legacyKeyID              = "legacy-v1"
	defaultDBMaxOpenConns    = 5
	defaultDBMaxIdleConns    = 2
	defaultDBConnMaxLifetime = 30 * time.Minute
)

type Config struct {
	Environment                string
	Port                       string
	DatabaseURL                string
	DBMaxOpenConns             int
	DBMaxIdleConns             int
	DBConnMaxLifetime          time.Duration
	AllowedCallers             []string
	AllowDevCallerHeader       bool
	ActiveEncryptionKeyID      string
	EncryptionKeys             map[string][]byte
	DataEncryptionKey          []byte
	ActiveHashKeyID            string
	HashKeys                   map[string][]byte
	HashKey                    []byte
	QueueDriver                string
	ServiceBusNamespace        string
	ServiceBusQueueName        string
	ServiceBusConnectionString string
	SMTPAddr                   string
	SMTPUsername               string
	SMTPPassword               string
	SMTPFrom                   string
	SMTPFromName               string
	NotificationsDisabled      bool
	TemplateDailyLimit         int
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
	templateDailyLimit, err := intEnv("NOTIFICATION_TEMPLATE_DAILY_LIMIT", 1_000)
	if err != nil {
		return Config{}, err
	}
	if templateDailyLimit <= 0 {
		return Config{}, fmt.Errorf("NOTIFICATION_TEMPLATE_DAILY_LIMIT must be positive")
	}
	shutdownTimeoutSeconds, err := intEnv("SHUTDOWN_TIMEOUT_SECONDS", 30)
	if err != nil {
		return Config{}, err
	}
	if shutdownTimeoutSeconds <= 0 {
		return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT_SECONDS must be positive")
	}
	dbMaxOpenConns, err := intEnv("DB_MAX_OPEN_CONNS", defaultDBMaxOpenConns)
	if err != nil {
		return Config{}, err
	}
	if dbMaxOpenConns <= 0 {
		return Config{}, fmt.Errorf("DB_MAX_OPEN_CONNS must be positive")
	}
	dbMaxIdleConns, err := intEnv("DB_MAX_IDLE_CONNS", defaultDBMaxIdleConns)
	if err != nil {
		return Config{}, err
	}
	if dbMaxIdleConns <= 0 {
		return Config{}, fmt.Errorf("DB_MAX_IDLE_CONNS must be positive")
	}
	if dbMaxIdleConns > dbMaxOpenConns {
		return Config{}, fmt.Errorf("DB_MAX_IDLE_CONNS must not exceed DB_MAX_OPEN_CONNS")
	}
	dbConnMaxLifetime, err := durationEnv("DB_CONN_MAX_LIFETIME", defaultDBConnMaxLifetime)
	if err != nil {
		return Config{}, err
	}
	if dbConnMaxLifetime <= 0 {
		return Config{}, fmt.Errorf("DB_CONN_MAX_LIFETIME must be positive")
	}

	activeEncryptionKeyID, encryptionKeys, dataEncryptionKey, err := loadEncryptionKeys()
	if err != nil {
		return Config{}, err
	}
	activeHashKeyID, hashKeys, hashKey, err := loadHashKeys()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Environment:                env("ENVIRONMENT", EnvironmentDevelopment),
		Port:                       env("PORT", "8081"),
		DatabaseURL:                os.Getenv("DATABASE_URL"),
		DBMaxOpenConns:             dbMaxOpenConns,
		DBMaxIdleConns:             dbMaxIdleConns,
		DBConnMaxLifetime:          dbConnMaxLifetime,
		AllowedCallers:             parseCallers(os.Getenv("NOTIFICATION_ALLOWED_CALLERS")),
		AllowDevCallerHeader:       allowDevCallerHeader,
		ActiveEncryptionKeyID:      activeEncryptionKeyID,
		EncryptionKeys:             encryptionKeys,
		DataEncryptionKey:          dataEncryptionKey,
		ActiveHashKeyID:            activeHashKeyID,
		HashKeys:                   hashKeys,
		HashKey:                    hashKey,
		QueueDriver:                env("QUEUE_DRIVER", "servicebus"),
		ServiceBusNamespace:        os.Getenv("SERVICEBUS_NAMESPACE"),
		ServiceBusQueueName:        env("SERVICEBUS_QUEUE_NAME", "notifications-email"),
		ServiceBusConnectionString: os.Getenv("SERVICEBUS_CONNECTION_STRING"),
		SMTPAddr:                   os.Getenv("SMTP_ADDR"),
		SMTPUsername:               os.Getenv("SMTP_USERNAME"),
		SMTPPassword:               os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:                   os.Getenv("SMTP_FROM"),
		SMTPFromName:               env("SMTP_FROM_NAME", "哈利路亞家教會"),
		NotificationsDisabled:      notificationsDisabled,
		TemplateDailyLimit:         templateDailyLimit,
		ShutdownTimeout:            time.Duration(shutdownTimeoutSeconds) * time.Second,
	}

	if err := cfg.validateLoaded(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validateLoaded() error {
	if c.Environment != EnvironmentDevelopment && c.Environment != EnvironmentProduction {
		return fmt.Errorf("ENVIRONMENT must be development or production")
	}
	return nil
}

func (c Config) Validate(mode string) error {
	if err := c.validateLoaded(); err != nil {
		return err
	}
	if c.Environment == EnvironmentProduction {
		if c.ServiceBusConnectionString != "" {
			return fmt.Errorf("SERVICEBUS_CONNECTION_STRING is development-only")
		}
		if c.AllowDevCallerHeader {
			return fmt.Errorf("NOTIFICATION_ALLOW_DEV_CALLER_HEADER must be false in production")
		}
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	switch mode {
	case "migrate":
		return nil
	case "api":
		if err := c.validateEncryptionAndServiceBus(); err != nil {
			return err
		}
		if len(c.HashKey) < 32 {
			return fmt.Errorf("NOTIFICATION_HASH_KEY must contain at least 32 bytes")
		}
		if len(c.AllowedCallers) == 0 {
			return fmt.Errorf("NOTIFICATION_ALLOWED_CALLERS is required")
		}
		return nil
	case "worker":
		if err := c.validateEncryptionAndServiceBus(); err != nil {
			return err
		}
		if c.SMTPAddr == "" || c.SMTPFrom == "" {
			return fmt.Errorf("SMTP_ADDR and SMTP_FROM are required")
		}
		return nil
	default:
		return fmt.Errorf("mode must be api, worker, or migrate")
	}
}

func (c Config) validateEncryptionAndServiceBus() error {
	if len(c.DataEncryptionKey) != 32 {
		return fmt.Errorf("NOTIFICATION_DATA_ENCRYPTION_KEY must decode to 32 bytes")
	}
	if c.QueueDriver != "servicebus" {
		return fmt.Errorf("QUEUE_DRIVER must be servicebus")
	}
	if c.ServiceBusNamespace == "" && c.ServiceBusConnectionString == "" {
		return fmt.Errorf("SERVICEBUS_NAMESPACE or SERVICEBUS_CONNECTION_STRING is required")
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

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	return parsed, nil
}

func loadEncryptionKeys() (string, map[string][]byte, []byte, error) {
	activeID := os.Getenv("NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID")
	raw := os.Getenv("NOTIFICATION_ENCRYPTION_KEYS_JSON")
	if activeID == "" && raw == "" {
		value := os.Getenv("NOTIFICATION_DATA_ENCRYPTION_KEY")
		if value == "" {
			return "", nil, nil, nil
		}
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return "", nil, nil, fmt.Errorf("NOTIFICATION_DATA_ENCRYPTION_KEY must be base64: %w", err)
		}
		if len(key) != 32 {
			return "", nil, nil, fmt.Errorf("NOTIFICATION_DATA_ENCRYPTION_KEY must decode to 32 bytes")
		}
		return legacyKeyID, map[string][]byte{legacyKeyID: key}, key, nil
	}
	if activeID == "" || raw == "" {
		return "", nil, nil, fmt.Errorf(
			"NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID and NOTIFICATION_ENCRYPTION_KEYS_JSON must be set together",
		)
	}
	values, err := parseJSONKeyMap("NOTIFICATION_ENCRYPTION_KEYS_JSON", raw)
	if err != nil {
		return "", nil, nil, err
	}
	keys := make(map[string][]byte, len(values))
	for id, value := range values {
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return "", nil, nil, fmt.Errorf("NOTIFICATION_ENCRYPTION_KEYS_JSON key %q must be base64: %w", id, err)
		}
		if len(key) != 32 {
			return "", nil, nil, fmt.Errorf("NOTIFICATION_ENCRYPTION_KEYS_JSON key %q must decode to 32 bytes", id)
		}
		keys[id] = key
	}
	key, ok := keys[activeID]
	if !ok {
		return "", nil, nil, fmt.Errorf("NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID active key %q is missing", activeID)
	}
	if legacy := os.Getenv("NOTIFICATION_DATA_ENCRYPTION_KEY"); legacy != "" {
		legacyKey, err := base64.StdEncoding.DecodeString(legacy)
		if err != nil {
			return "", nil, nil, fmt.Errorf("NOTIFICATION_DATA_ENCRYPTION_KEY must be base64: %w", err)
		}
		if !bytes.Equal(legacyKey, keys[legacyKeyID]) {
			return "", nil, nil, fmt.Errorf("NOTIFICATION_DATA_ENCRYPTION_KEY must match NOTIFICATION_ENCRYPTION_KEYS_JSON key %q", legacyKeyID)
		}
	}
	return activeID, keys, key, nil
}

func loadHashKeys() (string, map[string][]byte, []byte, error) {
	activeID := os.Getenv("NOTIFICATION_ACTIVE_HASH_KEY_ID")
	raw := os.Getenv("NOTIFICATION_HASH_KEYS_JSON")
	if activeID == "" && raw == "" {
		value := os.Getenv("NOTIFICATION_HASH_KEY")
		if value == "" {
			return "", nil, nil, nil
		}
		key := []byte(value)
		if len(key) < 32 {
			return "", nil, nil, fmt.Errorf("NOTIFICATION_HASH_KEY must contain at least 32 bytes")
		}
		return legacyKeyID, map[string][]byte{legacyKeyID: key}, key, nil
	}
	if activeID == "" || raw == "" {
		return "", nil, nil, fmt.Errorf(
			"NOTIFICATION_ACTIVE_HASH_KEY_ID and NOTIFICATION_HASH_KEYS_JSON must be set together",
		)
	}
	values, err := parseJSONKeyMap("NOTIFICATION_HASH_KEYS_JSON", raw)
	if err != nil {
		return "", nil, nil, err
	}
	keys := make(map[string][]byte, len(values))
	for id, value := range values {
		key := []byte(value)
		if len(key) < 32 {
			return "", nil, nil, fmt.Errorf("NOTIFICATION_HASH_KEYS_JSON key %q must contain at least 32 bytes", id)
		}
		keys[id] = key
	}
	key, ok := keys[activeID]
	if !ok {
		return "", nil, nil, fmt.Errorf("NOTIFICATION_ACTIVE_HASH_KEY_ID active key %q is missing", activeID)
	}
	if legacy := os.Getenv("NOTIFICATION_HASH_KEY"); legacy != "" &&
		!bytes.Equal([]byte(legacy), keys[legacyKeyID]) {
		return "", nil, nil, fmt.Errorf("NOTIFICATION_HASH_KEY must match NOTIFICATION_HASH_KEYS_JSON key %q", legacyKeyID)
	}
	return activeID, keys, key, nil
}

func parseJSONKeyMap(name, raw string) (map[string]string, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("%s must be a JSON object: %w", name, err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("%s must be a JSON object", name)
	}

	values := make(map[string]string)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("%s contains an invalid key ID: %w", name, err)
		}
		id, ok := token.(string)
		if !ok || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%s contains an empty key ID", name)
		}
		if _, exists := values[id]; exists {
			return nil, fmt.Errorf("%s contains duplicate key ID %q", name, id)
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("%s key %q must contain a string: %w", name, id, err)
		}
		values[id] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object: %w", name, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%s must contain one JSON object", name)
		}
		return nil, fmt.Errorf("%s must contain one JSON object: %w", name, err)
	}
	return values, nil
}
