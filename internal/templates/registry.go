package templates

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
)

var (
	ErrUnknownTemplate    = errors.New("unknown template")
	ErrUnsupportedChannel = errors.New("unsupported channel")
	ErrForbiddenCaller    = errors.New("forbidden template caller")
	ErrInvalidPayload     = errors.New("invalid template payload")
)

type Definition struct {
	ID              string
	Version         int
	Channel         string
	AllowedCallers  map[string]bool
	RequiredFields  map[string]bool
	AllowedFields   map[string]bool
	SupportedLocale map[string]bool
}

var definitions = map[string]Definition{
	"account.verify-email": {
		ID:              "account.verify-email",
		Version:         1,
		Channel:         "email",
		AllowedCallers:  set("account-api"),
		RequiredFields:  set("verifyUrl"),
		AllowedFields:   set("verifyUrl"),
		SupportedLocale: set("zh-Hant", "zh-Hans", "en"),
	},
	"account.reset-password": {
		ID:              "account.reset-password",
		Version:         1,
		Channel:         "email",
		AllowedCallers:  set("account-api"),
		RequiredFields:  set("resetUrl"),
		AllowedFields:   set("resetUrl"),
		SupportedLocale: set("zh-Hant", "zh-Hans", "en"),
	},
}

func Resolve(templateID, channel string) (Definition, error) {
	definition, ok := definitions[templateID]
	if !ok {
		return Definition{}, fmt.Errorf("%w: %s", ErrUnknownTemplate, templateID)
	}
	if definition.Channel != channel {
		return Definition{}, fmt.Errorf("%w: %s", ErrUnsupportedChannel, channel)
	}
	return cloneDefinition(definition), nil
}

func Validate(definition Definition, caller string, request contracts.SendRequest) (map[string]string, error) {
	canonical, err := Resolve(definition.ID, definition.Channel)
	if err != nil {
		return nil, err
	}
	if request.TemplateID != canonical.ID || request.Channel != canonical.Channel {
		return nil, fmt.Errorf("%w: template does not match request", ErrInvalidPayload)
	}
	if !canonical.AllowedCallers[caller] {
		return nil, fmt.Errorf("%w: %s", ErrForbiddenCaller, caller)
	}
	return validatePayload(canonical, request.Payload)
}

func validatePayload(definition Definition, payload map[string]string) (map[string]string, error) {
	validated := make(map[string]string, len(payload))
	for key, value := range payload {
		if !definition.AllowedFields[key] {
			return nil, fmt.Errorf("%w: unexpected field %q", ErrInvalidPayload, key)
		}
		parsed, err := url.ParseRequestURI(value)
		if err != nil || parsed.Host == "" || parsed.User != nil {
			return nil, fmt.Errorf("%w: %s must be an approved URL", ErrInvalidPayload, key)
		}
		host := strings.ToLower(parsed.Hostname())
		isHHC := host == "alive.org.tw" || strings.HasSuffix(host, ".alive.org.tw")
		isLocal := host == "localhost" || host == "127.0.0.1" || host == "::1"
		if (parsed.Scheme != "https" || !isHHC) && (parsed.Scheme != "http" || !isLocal) {
			return nil, fmt.Errorf("%w: %s must use an approved origin", ErrInvalidPayload, key)
		}
		validated[key] = parsed.String()
	}
	for key := range definition.RequiredFields {
		if validated[key] == "" {
			return nil, fmt.Errorf("%w: %s is required", ErrInvalidPayload, key)
		}
	}
	return validated, nil
}

func cloneDefinition(definition Definition) Definition {
	definition.AllowedCallers = cloneSet(definition.AllowedCallers)
	definition.RequiredFields = cloneSet(definition.RequiredFields)
	definition.AllowedFields = cloneSet(definition.AllowedFields)
	definition.SupportedLocale = cloneSet(definition.SupportedLocale)
	return definition
}

func set(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func cloneSet(source map[string]bool) map[string]bool {
	clone := make(map[string]bool, len(source))
	for value, present := range source {
		clone[value] = present
	}
	return clone
}
