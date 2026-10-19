package templates

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
)

func TestOAuthResendTemplateExpiry(t *testing.T) {
	def := mustResolve(t, "account.oauth-onboarding-code")
	if def.Version != 4 || def.TTL != 30*time.Minute {
		t.Fatalf("active definition = %#v", def)
	}
	for _, locale := range []string{"zh-Hant", "zh-Hans", "en"} {
		payload := map[string]string{"code": "123456", "provider": "google"}
		email, err := RenderEmail(def, locale, "user@example.test", payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{email.Body, email.HTMLBody} {
			if !strings.Contains(body, "30") || strings.Contains(body, "10 分") || strings.Contains(body, "10 minutes") {
				t.Fatalf("incorrect expiry: %s", body)
			}
		}
		payload["expiresAt"] = "2026-10-20T18:30:00+08:00"
		email, err = RenderEmail(def, locale, "user@example.test", payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{email.Body, email.HTMLBody} {
			if !strings.Contains(body, "2026-10-20T10:30:00Z") || strings.Contains(body, "30 minutes") || strings.Contains(body, "30 分") {
				t.Fatalf("missing absolute expiry: %s", body)
			}
		}
	}
	legacy := mustResolveVersion(t, "account.oauth-onboarding-code", 2)
	email, err := RenderEmail(legacy, "en", "user@example.test", map[string]string{"code": "123456", "provider": "google"})
	if err != nil || legacy.TTL != 15*time.Minute || !strings.Contains(email.Body, "10 minutes") {
		t.Fatalf("historical template changed: %#v %v", email, err)
	}
}

func TestOAuthResendDeadlineValidation(t *testing.T) {
	def := mustResolve(t, "account.oauth-onboarding-code")
	expiry := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC)
	request := contracts.SendRequest{TemplateID: def.ID, Channel: "email", ExpiresAt: &expiry, Payload: map[string]string{"code": "123456", "provider": "google", "expiresAt": "2026-10-20T18:30:00+08:00"}}
	if _, err := Validate(def, "account-api", request); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "bad", "0001-01-01T00:00:00Z", "2026-10-20T10:31:00Z"} {
		request.Payload["expiresAt"] = value
		if _, err := Validate(def, "account-api", request); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("accepted deadline %q: %v", value, err)
		}
	}
	request.Payload["expiresAt"] = expiry.Format(time.RFC3339)
	request.ExpiresAt = nil
	if _, err := Validate(def, "account-api", request); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("accepted missing delivery deadline: %v", err)
	}
	delete(request.Payload, "expiresAt")
	if _, err := Validate(def, "account-api", request); err != nil {
		t.Fatalf("old Account request rejected: %v", err)
	}
}
