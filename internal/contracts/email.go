package contracts

import (
	"net/mail"
	"strings"
)

func ValidEmail(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(normalized)
	return err == nil && parsed.Address == normalized
}
