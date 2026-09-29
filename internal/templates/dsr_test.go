package templates

import (
	"errors"
	"strings"
	"testing"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
)

func dsrPayload() map[string]string {
	return map[string]string{"requestUrl": "https://account.alive.org.tw/data-requests", "requestId": "0199bb0e-10a0-7e07-b25e-221d331b6d13", "requestType": "access_export"}
}

func TestDSRLifecycleTemplates(t *testing.T) {
	for _, id := range []string{"account.dsr-received", "account.dsr-information-required", "account.dsr-completed", "account.dsr-action-required"} {
		t.Run(id, func(t *testing.T) {
			definition, err := Resolve(id, "email")
			if err != nil {
				t.Fatal(err)
			}
			request := contracts.SendRequest{TemplateID: id, Channel: "email", Payload: dsrPayload()}
			if _, err := Validate(definition, "account-api", request); err != nil {
				t.Fatal(err)
			}
			if _, err := Validate(definition, "engagement-api", request); !errors.Is(err, ErrForbiddenCaller) {
				t.Fatalf("unexpected caller accepted: %v", err)
			}
			for _, locale := range []string{"zh-Hant", "zh-Hans", "en", "unsupported"} {
				email, err := RenderEmail(definition, locale, "user@example.test", dsrPayload())
				if err != nil {
					t.Fatal(err)
				}
				if email.Subject == "" || !strings.Contains(email.Body, dsrPayload()["requestId"]) || !strings.Contains(email.HTMLBody, "https://account.alive.org.tw/data-requests") {
					t.Fatalf("missing lifecycle content: %+v", email)
				}
				if email.ListUnsubscribe != "" || email.OneClickUnsubscribe {
					t.Fatal("transactional email has marketing opt-out")
				}
			}
		})
	}
}

func TestDSRPayloadRejectsUntrustedOrSensitiveFields(t *testing.T) {
	definition, err := Resolve("account.dsr-received", "email")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, value string }{
		{"requestId", "not-a-uuid"}, {"requestType", "unknown"}, {"internalNote", "private"}, {"supplement", "private"},
		{"requestUrl", "https://evil.test/data-requests"}, {"requestUrl", "https://www.alive.org.tw/data-requests"},
		{"requestUrl", "https://account.alive.org.tw/data-requests?token=secret"}, {"requestUrl", "https://account.alive.org.tw/data-requests#secret"},
		{"requestUrl", "https://account.alive.org.tw/other"},
	} {
		payload := dsrPayload()
		payload[tc.key] = tc.value
		_, err := validatePayload(definition, payload)
		if !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("accepted %s %s: %v", tc.key, tc.value, err)
		}
	}
	for key := range dsrPayload() {
		payload := dsrPayload()
		delete(payload, key)
		if _, err := validatePayload(definition, payload); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("accepted missing %s", key)
		}
	}
}

func TestErasureCompletionDoesNotRequireDeletedAccount(t *testing.T) {
	definition, err := Resolve("account.dsr-completed", "email")
	if err != nil {
		t.Fatal(err)
	}
	payload := dsrPayload()
	payload["requestType"] = "erasure"
	for _, locale := range []string{"zh-Hant", "zh-Hans", "en"} {
		email, err := RenderEmail(definition, locale, "user@example.test", payload)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(email.Body, "/data-requests") || strings.Contains(email.HTMLBody, "/data-requests") {
			t.Fatal("deleted user instructed to sign in")
		}
		if strings.Contains(email.HTMLBody, "<a ") {
			t.Fatal("erasure completion contains an action button")
		}
		if !strings.Contains(email.Body, "support@alive.org.tw") {
			t.Fatal("missing support contact")
		}
	}
}
