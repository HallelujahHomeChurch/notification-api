package templates

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
)

func correctionRequest(t *testing.T, kind string) contracts.SendRequest {
	t.Helper()
	const id = "3f6b02b2-58ef-4337-ad61-96bdfb588a65"
	field, path := "actionUrl", "https://account.alive.org.tw/security"
	switch kind {
	case "verify":
		field, path = "verifyUrl", "https://account.alive.org.tw/data-requests/verify-email#token=opaque-token"
	case "admin-review":
		field, path = "reviewUrl", "https://admin.alive.org.tw/data-requests/"+id
	case "completed":
		path = "https://account.alive.org.tw/login"
	}
	var request contracts.SendRequest
	if err := json.Unmarshal([]byte(`{"expiresAt":"2026-10-16T12:00:00Z"}`), &request); err != nil {
		t.Fatal(err)
	}
	request.TemplateID = "account.email-correction-" + kind
	request.Channel, request.Locale = "email", "en"
	request.SubjectAccountID = "bf9c8d16-0c10-4841-aabd-ae308b8dd3ac"
	request.Resource = contracts.Resource{Type: "email_correction", ID: id}
	request.Payload = map[string]string{field: path}
	return request
}

func TestEmailCorrectionTemplates(t *testing.T) {
	for _, kind := range []string{"verify", "security-notice", "admin-review", "completed"} {
		t.Run(kind, func(t *testing.T) {
			request := correctionRequest(t, kind)
			def, err := Resolve(request.TemplateID, "email")
			if err != nil {
				t.Fatal(err)
			}
			ttl := 24 * time.Hour
			if kind == "verify" {
				ttl = 30 * time.Minute
			}
			if def.Version != 1 || def.TTL != ttl {
				t.Fatalf("definition = %#v", def)
			}
			if _, err := Resolve(request.TemplateID, "web_push"); !errors.Is(err, ErrUnsupportedChannel) {
				t.Fatalf("channel error %v", err)
			}
			if _, err := Validate(def, "engagement-api", request); !errors.Is(err, ErrForbiddenCaller) {
				t.Fatalf("caller error %v", err)
			}
			subjects := map[string]bool{}
			for _, locale := range []string{"zh-Hant", "zh-Hans", "en", "ja", "ko"} {
				request.Locale = locale
				if _, err := Validate(def, "account-api", request); err != nil {
					t.Fatal(err)
				}
				email, err := RenderEmail(def, locale, "recipient@example.test", request.Payload)
				if err != nil {
					t.Fatal(err)
				}
				if email.Body == "" || !strings.Contains(email.HTMLBody, `lang="`+locale+`"`) || !strings.Contains(email.HTMLBody, "font-family:-apple-system") {
					t.Fatalf("missing localized shell for %s", locale)
				}
				subjects[email.Subject+email.Body] = true
			}
			if len(subjects) != 5 {
				t.Fatalf("localized subjects: %v", subjects)
			}
			request.Payload["body"] = "untrusted freeform"
			if _, err := Validate(def, "account-api", request); !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("extra field error %v", err)
			}
		})
	}
}

func TestEmailCorrectionURLAndSubjectBoundary(t *testing.T) {
	for _, kind := range []string{"verify", "security-notice", "admin-review", "completed"} {
		request := correctionRequest(t, kind)
		def, err := Resolve(request.TemplateID, "email")
		if err != nil {
			t.Fatal(err)
		}
		for _, subject := range []string{"", "00000000-0000-0000-0000-000000000000", "bf9c8d160c104841aabdae308b8dd3ac"} {
			bad := request
			bad.SubjectAccountID = subject
			if _, err := Validate(def, "account-api", bad); !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("%s subject %q: %v", kind, subject, err)
			}
		}
		for _, resource := range []contracts.Resource{{Type: "account", ID: request.Resource.ID}, {Type: "email_correction", ID: "00000000-0000-0000-0000-000000000000"}, {Type: "email_correction", ID: "not-a-uuid"}} {
			bad := request
			bad.Resource = resource
			if _, err := Validate(def, "account-api", bad); !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("%s resource %#v: %v", kind, resource, err)
			}
		}
		for field, validURL := range request.Payload {
			for _, badURL := range []string{strings.Replace(validURL, "account.alive.org.tw", "www.alive.org.tw", 1), strings.Replace(validURL, "admin.alive.org.tw", "www.alive.org.tw", 1), validURL + "?redirect=evil", strings.Replace(validURL, ".alive.org.tw/", ".alive.org.tw:444/", 1), strings.Replace(validURL, "/data-requests/", "/data-requests/%2e%2e/", 1)} {
				if badURL == validURL {
					continue
				}
				if _, err := RenderEmail(def, "en", "recipient@example.test", map[string]string{field: badURL}); !errors.Is(err, ErrInvalidPayload) {
					t.Fatalf("%s accepted URL %q: %v", kind, badURL, err)
				}
			}
		}
	}
	request := correctionRequest(t, "verify")
	def, _ := Resolve(request.TemplateID, "email")
	for _, raw := range []string{"https://account.alive.org.tw/data-requests/verify-email?token=opaque", "https://account.alive.org.tw/data-requests/verify-email#token=opaque&next=evil", "https://account.alive.org.tw/data-requests/verify-email#token=", "https://account.alive.org.tw/data-requests/verify-email#token=%22%3E%3Cscript%3E"} {
		if _, err := RenderEmail(def, "en", "recipient@example.test", map[string]string{"verifyUrl": raw}); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("accepted verify %q: %v", raw, err)
		}
	}
}

func TestEmailCorrectionRejectsMissingPayloadAndMismatchedReview(t *testing.T) {
	for _, kind := range []string{"verify", "security-notice", "admin-review", "completed"} {
		request := correctionRequest(t, kind)
		def, _ := Resolve(request.TemplateID, "email")
		request.Payload = nil
		if _, err := Validate(def, "account-api", request); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("%s missing field: %v", kind, err)
		}
	}
	request := correctionRequest(t, "admin-review")
	def, _ := Resolve(request.TemplateID, "email")
	request.Resource.ID = "bf9c8d16-0c10-4841-aabd-ae308b8dd3ac"
	if _, err := Validate(def, "account-api", request); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("mismatched review: %v", err)
	}
	request = correctionRequest(t, "verify")
	def, _ = Resolve(request.TemplateID, "email")
	zero := time.Time{}
	request.ExpiresAt = &zero
	if _, err := Validate(def, "account-api", request); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("zero expiry: %v", err)
	}
}

func TestEmailCorrectionShellEscapesURL(t *testing.T) {
	for _, locale := range []string{"zh-Hant", "zh-Hans", "en", "ja", "ko"} {
		email := renderEmailCorrection("account.email-correction-verify", locale, "recipient@example.test", map[string]string{"verifyUrl": `https://account.alive.org.tw/" onclick="bad<>&`})
		if strings.Contains(email.HTMLBody, `" onclick="`) || !strings.Contains(email.HTMLBody, "&#34;") || !strings.Contains(email.HTMLBody, "&lt;&gt;&amp;") {
			t.Fatalf("unescaped shell for %s", locale)
		}
	}
}
