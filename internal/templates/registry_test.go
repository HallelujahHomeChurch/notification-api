package templates

import (
	"errors"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
)

func TestResolveAccountTemplates(t *testing.T) {
	ttls := map[string]time.Duration{
		"account.verify-email":            24 * time.Hour,
		"account.reset-password":          time.Hour,
		"account.oauth-link-confirmation": 15 * time.Minute,
		"account.oauth-onboarding-code":   15 * time.Minute,
	}
	for templateID, ttl := range ttls {
		definition, err := Resolve(templateID, "email")
		if err != nil {
			t.Fatalf("Resolve(%q, email) error = %v", templateID, err)
		}
		wantVersion := 2
		if definition.Version != wantVersion {
			t.Fatalf("Resolve(%q, email).Version = %d, want %d", templateID, definition.Version, wantVersion)
		}
		if definition.Channel != "email" {
			t.Fatalf("Resolve(%q, email).Channel = %q, want email", templateID, definition.Channel)
		}
		if definition.TTL != ttl {
			t.Fatalf("Resolve(%q, email).TTL = %s, want %s", templateID, definition.TTL, ttl)
		}
	}
}

func TestValidateNewsletterTemplate(t *testing.T) {
	definition, err := Resolve("engagement.newsletter", "email")
	if err != nil {
		t.Fatal(err)
	}
	request := verificationRequest(map[string]string{
		"subject": "August news", "body": "Church updates",
		"unsubscribeUrl":         "https://www.alive.org.tw/zh-Hant/newsletter/unsubscribe?token=opaque",
		"oneClickUnsubscribeUrl": "https://www.alive.org.tw/api/engagement/v1/newsletter/unsubscribe?token=opaque",
	})
	request.TemplateID = definition.ID
	if _, err := Validate(definition, "engagement-api", request); err != nil {
		t.Fatal(err)
	}
	request.Payload["subject"] = "unsafe\r\nBcc: other@example.test"
	if _, err := Validate(definition, "engagement-api", request); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("header injection error = %v", err)
	}
	delete(request.Payload, "oneClickUnsubscribeUrl")
	if _, err := Validate(definition, "engagement-api", request); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("missing one-click unsubscribe URL error = %v", err)
	}
}

func TestValidateWebPushTemplate(t *testing.T) {
	definition, err := Resolve("engagement.web-push", "web_push")
	if err != nil {
		t.Fatal(err)
	}
	request := contracts.SendRequest{
		TemplateID: definition.ID,
		Channel:    definition.Channel,
		Payload: map[string]string{
			"title":         "August news",
			"body":          "Church updates",
			"clickBehavior": "url",
			"actionUrl":     "https://www.alive.org.tw/zh-Hant/news",
		},
	}
	if _, err := Validate(definition, "engagement-api", request); err != nil {
		t.Fatal(err)
	}
	request.Payload["title"] = "unsafe\r\nheader"
	if _, err := Validate(definition, "engagement-api", request); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("header injection error = %v", err)
	}
}

func TestValidateWebPushClickBehavior(t *testing.T) {
	definition, err := Resolve("engagement.web-push", "web_push")
	if err != nil {
		t.Fatal(err)
	}
	base := contracts.SendRequest{TemplateID: definition.ID, Channel: definition.Channel}
	for _, test := range []struct {
		name    string
		payload map[string]string
		valid   bool
	}{
		{name: "home", payload: map[string]string{"title": "消息", "body": "內容", "clickBehavior": "home"}, valid: true},
		{name: "dismiss", payload: map[string]string{"title": "消息", "body": "內容", "clickBehavior": "dismiss"}, valid: true},
		{name: "url", payload: map[string]string{"title": "消息", "body": "內容", "clickBehavior": "url", "actionUrl": "https://www.alive.org.tw/zh-Hant/news"}, valid: true},
		{name: "url missing target", payload: map[string]string{"title": "消息", "body": "內容", "clickBehavior": "url"}},
		{name: "dismiss with target", payload: map[string]string{"title": "消息", "body": "內容", "clickBehavior": "dismiss", "actionUrl": "https://www.alive.org.tw/zh-Hant/news"}},
		{name: "unknown", payload: map[string]string{"title": "消息", "body": "內容", "clickBehavior": "unknown"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Payload = test.payload
			_, err := Validate(definition, "engagement-api", request)
			if (err == nil) != test.valid {
				t.Fatalf("Validate() error = %v, valid = %v", err, test.valid)
			}
		})
	}
}

func TestValidateOAuthOnboardingCode(t *testing.T) {
	definition := mustResolve(t, "account.oauth-onboarding-code")
	request := verificationRequest(map[string]string{"code": "123456", "provider": "line"})
	request.TemplateID = definition.ID

	if _, err := Validate(definition, "account-api", request); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	for _, code := range []string{"12345", "1234567", "abcdef"} {
		request.Payload["code"] = code
		if _, err := Validate(definition, "account-api", request); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("Validate(code=%q) error = %v, want ErrInvalidPayload", code, err)
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
