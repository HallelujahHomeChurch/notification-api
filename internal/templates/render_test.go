package templates

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderEmailLocalizesAccountVerification(t *testing.T) {
	definition, err := ResolveVersion("account.verify-email", 1, "email")
	if err != nil {
		t.Fatal(err)
	}
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

func TestRenderCurrentVerificationEmailHasBrandedHTMLAndPlainTextFallback(t *testing.T) {
	definition := mustResolve(t, "account.verify-email")
	verifyURL := "https://account.alive.org.tw/verify-email?token=opaque"
	email, err := RenderEmail(definition, "zh-Hant", "user@example.test", map[string]string{"verifyUrl": verifyURL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(email.Body, verifyURL) {
		t.Fatalf("plain text body must contain fallback URL: %q", email.Body)
	}
	for _, want := range []string{"哈利路亞家教會", "驗證 Email", `href="` + verifyURL + `"`} {
		if !strings.Contains(email.HTMLBody, want) {
			t.Fatalf("HTML body missing %q", want)
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

func TestRenderOAuthLinkConfirmation(t *testing.T) {
	definition := mustResolve(t, "account.oauth-link-confirmation")
	payload := map[string]string{
		"confirmUrl": "https://account.alive.org.tw/oauth/link#token=opaque",
		"provider":   "line",
	}

	for _, test := range []struct {
		locale  string
		subject string
		body    string
	}{
		{"zh-Hant", "確認連結 LINE 登入", "如果您剛剛要求將 LINE 連結到 HHC 帳戶，請使用以下連結確認：\n\nhttps://account.alive.org.tw/oauth/link#token=opaque\n"},
		{"zh-Hans", "确认关联 LINE 登录", "如果您刚刚要求将 LINE 关联到 HHC 帐户，请使用以下链接确认：\n\nhttps://account.alive.org.tw/oauth/link#token=opaque\n"},
		{"en", "Confirm LINE sign-in link", "If you requested to link LINE to your HHC account, confirm it using this link:\n\nhttps://account.alive.org.tw/oauth/link#token=opaque\n"},
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

func TestRenderOAuthOnboardingCode(t *testing.T) {
	definition := mustResolve(t, "account.oauth-onboarding-code")
	email, err := RenderEmail(definition, "zh-Hant", "user@example.test", map[string]string{
		"code": "123456", "provider": "microsoft",
	})
	if err != nil {
		t.Fatalf("RenderEmail() error = %v", err)
	}
	if email.Subject != "驗證您的 HHC 帳戶 Email" {
		t.Fatalf("RenderEmail() subject = %q", email.Subject)
	}
	if email.Body != "您的 HHC 帳戶驗證碼是：123456\n\n驗證碼將於 10 分鐘後失效。請勿將驗證碼提供給他人。\n" {
		t.Fatalf("RenderEmail() body = %q", email.Body)
	}
}

func TestQueuedVersionRendersAfterNewVersionBecomesCurrent(t *testing.T) {
	templateID := "account.verify-email"
	originalCurrent := currentVersions[templateID]
	originalVersions := definitions[templateID]
	t.Cleanup(func() {
		currentVersions[templateID] = originalCurrent
		definitions[templateID] = originalVersions
	})

	v1, err := ResolveVersion(templateID, 1, "email")
	if err != nil {
		t.Fatalf("ResolveVersion(v1) error = %v", err)
	}
	v3 := cloneDefinition(v1)
	v3.Version = 3
	v3.RequiredFields = set("confirmationUrl")
	v3.AllowedFields = set("confirmationUrl")
	definitions[templateID] = map[int]Definition{1: v1, 3: v3}
	currentVersions[templateID] = 3

	current, err := Resolve(templateID, "email")
	if err != nil || current.Version != 3 {
		t.Fatalf("Resolve(current) = %#v, error = %v", current, err)
	}
	email, err := RenderEmail(v1, "en", "user@example.test", map[string]string{
		"verifyUrl": "https://account.alive.org.tw/verify-email?token=queued-v1",
	})
	if err != nil {
		t.Fatalf("RenderEmail(queued v1) error = %v", err)
	}
	if email.Subject != "Verify your HHC account" {
		t.Fatalf("RenderEmail(queued v1) subject = %q", email.Subject)
	}
	if _, err := RenderEmail(v3, "en", "user@example.test", map[string]string{
		"confirmationUrl": "https://account.alive.org.tw/verify-email?token=v3",
	}); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("RenderEmail(unimplemented v3) error = %v, want ErrUnknownTemplate", err)
	}
}
