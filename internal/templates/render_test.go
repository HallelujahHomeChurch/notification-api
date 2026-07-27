package templates

import "testing"

func TestRenderEmailLocalizesAccountVerification(t *testing.T) {
	definition := mustResolve(t, "account.verify-email")
	payload := map[string]string{"verifyUrl": "https://account.alive.org.tw/verify-email?token=opaque"}

	for _, test := range []struct {
		locale  string
		subject string
		body    string
	}{
		{"zh-Hant", "驗證您的 HHC 帳戶", "請使用以下連結驗證您的 HHC 帳戶：\n\nhttps://account.alive.org.tw/verify-email?token=opaque\n"},
		{"zh-Hans", "验证您的 HHC 帐户", "请使用以下链接验证您的 HHC 帐户：\n\nhttps://account.alive.org.tw/verify-email?token=opaque\n"},
		{"en", "Verify your HHC account", "Use this link to verify your HHC account:\n\nhttps://account.alive.org.tw/verify-email?token=opaque\n"},
	} {
		email, err := RenderEmail(definition, test.locale, "user@example.test", payload)
		if err != nil {
			t.Fatalf("RenderEmail(%q) error = %v", test.locale, err)
		}
		if email.Subject != test.subject || email.Body != test.body {
			t.Fatalf("RenderEmail(%q) = %#v, want subject=%q body=%q", test.locale, email, test.subject, test.body)
		}
	}
}

func TestRenderEmailFallsBackToEnglish(t *testing.T) {
	definition := mustResolve(t, "account.reset-password")
	email, err := RenderEmail(definition, "fr", "user@example.test", map[string]string{
		"resetUrl": "https://account.alive.org.tw/reset-password?token=opaque",
	})
	if err != nil {
		t.Fatalf("RenderEmail() error = %v", err)
	}
	if email.Subject != "Reset your HHC account password" {
		t.Fatalf("RenderEmail() subject = %q", email.Subject)
	}
	if email.Body != "Use this link to reset your HHC account password:\n\nhttps://account.alive.org.tw/reset-password?token=opaque\n" {
		t.Fatalf("RenderEmail() body = %q", email.Body)
	}
}
