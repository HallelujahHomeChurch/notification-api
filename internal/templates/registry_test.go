package templates

import (
	"errors"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
)

func TestResolveAccountTemplates(t *testing.T) {
	for _, templateID := range []string{"account.verify-email", "account.reset-password", "account.oauth-link-confirmation"} {
		definition, err := Resolve(templateID, "email")
		if err != nil {
			t.Fatalf("Resolve(%q, email) error = %v", templateID, err)
		}
		if definition.Version != 1 {
			t.Fatalf("Resolve(%q, email).Version = %d, want 1", templateID, definition.Version)
		}
		if definition.Channel != "email" {
			t.Fatalf("Resolve(%q, email).Channel = %q, want email", templateID, definition.Channel)
		}
	}
}

func TestValidateOAuthLinkConfirmation(t *testing.T) {
	definition := mustResolve(t, "account.oauth-link-confirmation")
	request := verificationRequest(map[string]string{
		"confirmUrl": "https://account.alive.org.tw/oauth/link#token=opaque",
		"provider":   "google",
	})
	request.TemplateID = definition.ID

	payload, err := Validate(definition, "account-api", request)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if payload["provider"] != "google" {
		t.Fatalf("Validate() payload = %#v", payload)
	}

	request.Payload["provider"] = "untrusted"
	if _, err := Validate(definition, "account-api", request); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("Validate() error = %v, want ErrInvalidPayload", err)
	}
}

func TestValidateAllowsAccountAPICaller(t *testing.T) {
	definition := mustResolve(t, "account.verify-email")
	payload, err := Validate(definition, "account-api", verificationRequest(map[string]string{
		"verifyUrl": "https://account.alive.org.tw/verify-email?token=opaque",
	}))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if payload["verifyUrl"] != "https://account.alive.org.tw/verify-email?token=opaque" {
		t.Fatalf("Validate() payload = %#v", payload)
	}
}

func TestValidateRejectsUnauthorizedCaller(t *testing.T) {
	definition := mustResolve(t, "account.verify-email")
	_, err := Validate(definition, "hhc-web-api", verificationRequest(map[string]string{
		"verifyUrl": "https://account.alive.org.tw/verify-email?token=opaque",
	}))
	if !errors.Is(err, ErrForbiddenCaller) {
		t.Fatalf("Validate() error = %v, want ErrForbiddenCaller", err)
	}
}

func TestValidateRejectsUnknownPayloadField(t *testing.T) {
	definition := mustResolve(t, "account.verify-email")
	_, err := Validate(definition, "account-api", verificationRequest(map[string]string{
		"verifyUrl": "https://account.alive.org.tw/verify-email?token=opaque",
		"subject":   "caller controlled",
	}))
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("Validate() error = %v, want ErrInvalidPayload", err)
	}
}

func TestValidateRejectsMissingRequiredURL(t *testing.T) {
	definition := mustResolve(t, "account.reset-password")
	request := verificationRequest(map[string]string{})
	request.TemplateID = "account.reset-password"

	_, err := Validate(definition, "account-api", request)
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("Validate() error = %v, want ErrInvalidPayload", err)
	}
}

func TestValidateRejectsUnsafeURL(t *testing.T) {
	definition := mustResolve(t, "account.verify-email")
	_, err := Validate(definition, "account-api", verificationRequest(map[string]string{
		"verifyUrl": "javascript:alert(1)",
	}))
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("Validate() error = %v, want ErrInvalidPayload", err)
	}
}

func TestValidateAllowsHHCAndLocalDevelopmentOrigins(t *testing.T) {
	definition := mustResolve(t, "account.verify-email")
	for _, verifyURL := range []string{
		"https://alive.org.tw/verify-email?token=opaque",
		"https://account.alive.org.tw/verify-email?token=opaque",
		"https://account.alive.org.tw/verify-email#token=opaque",
		"http://localhost:5173/verify-email?token=opaque",
		"http://127.0.0.1:8080/verify-email?token=opaque",
		"http://[::1]:3000/verify-email?token=opaque",
	} {
		t.Run(verifyURL, func(t *testing.T) {
			if _, err := Validate(definition, "account-api", verificationRequest(map[string]string{"verifyUrl": verifyURL})); err != nil {
				t.Fatalf("Validate(%q) error = %v", verifyURL, err)
			}
		})
	}
}

func TestValidateRejectsUntrustedURLOrigins(t *testing.T) {
	definition := mustResolve(t, "account.verify-email")
	for _, verifyURL := range []string{
		"http://account.alive.org.tw/verify-email?token=opaque",
		"https://alive.org.tw.evil.test/verify-email?token=opaque",
		"https://user@account.alive.org.tw/verify-email?token=opaque",
		"https://example.test/verify-email?token=opaque",
	} {
		t.Run(verifyURL, func(t *testing.T) {
			_, err := Validate(definition, "account-api", verificationRequest(map[string]string{"verifyUrl": verifyURL}))
			if !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("Validate(%q) error = %v, want ErrInvalidPayload", verifyURL, err)
			}
		})
	}
}

func mustResolve(t *testing.T, templateID string) Definition {
	t.Helper()
	definition, err := Resolve(templateID, "email")
	if err != nil {
		t.Fatalf("Resolve(%q, email) error = %v", templateID, err)
	}
	return definition
}

func verificationRequest(payload map[string]string) contracts.SendRequest {
	return contracts.SendRequest{
		TemplateID: "account.verify-email",
		Channel:    "email",
		Target:     contracts.Target{Type: "email", Address: "user@example.test"},
		Locale:     "en",
		Payload:    payload,
		Resource:   contracts.Resource{Type: "account", ID: "user-id"},
	}
}
