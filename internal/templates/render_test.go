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

func TestRenderNewsletterEscapesContentAndAddsUnsubscribeMetadata(t *testing.T) {
	unsubscribeURL := "https://www.alive.org.tw/zh-Hant/newsletter/unsubscribe?token=opaque"
	oneClickURL := "https://www.alive.org.tw/api/engagement/v1/newsletter/unsubscribe?token=opaque"
	email, err := RenderEmail(mustResolve(t, "engagement.newsletter"), "zh-Hant", "user@example.test", map[string]string{
		"subject": "八月消息", "body": "第一行\n<script>alert(1)</script>", "unsubscribeUrl": unsubscribeURL,
		"oneClickUnsubscribeUrl": oneClickURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if email.Subject != "八月消息" || !strings.Contains(email.Body, "第一行") {
		t.Fatalf("email = %#v", email)
	}
	if strings.Contains(email.HTMLBody, "<script>") || !strings.Contains(email.HTMLBody, "&lt;script&gt;") {
		t.Fatalf("HTML was not escaped: %s", email.HTMLBody)
	}
	if email.ListUnsubscribe != "<"+oneClickURL+">" || !email.OneClickUnsubscribe {
		t.Fatalf("unsubscribe metadata = %#v", email)
	}
}

func TestRenderCurrentActionEmailsHaveBrandedHTML(t *testing.T) {
	tests := []struct {
		templateID string
		locale     string
		payload    map[string]string
		wants      []string
	}{
		{
			"account.reset-password", "zh-Hant",
			map[string]string{"resetUrl": "https://account.alive.org.tw/reset-password#token=opaque"},
			[]string{"哈利路亞家教會", "重設密碼", `href="https://account.alive.org.tw/reset-password#token=opaque"`},
		},
		{
			"account.oauth-link-confirmation", "en",
			map[string]string{"confirmUrl": "https://account.alive.org.tw/oauth/link#token=opaque", "provider": "line"},
			[]string{"Hallelujah Home Church", "Confirm LINE sign-in", `href="https://account.alive.org.tw/oauth/link#token=opaque"`},
		},
	}

	for _, test := range tests {
		email, err := RenderEmail(mustResolve(t, test.templateID), test.locale, "user@example.test", test.payload)
		if err != nil {
			t.Fatalf("RenderEmail(%q) error = %v", test.templateID, err)
		}
		for _, want := range test.wants {
			if !strings.Contains(email.HTMLBody, want) {
				t.Fatalf("RenderEmail(%q) HTML missing %q", test.templateID, want)
			}
		}
	}
}

func TestRenderCurrentOnboardingCodeHasBrandedHTML(t *testing.T) {
	email, err := RenderEmail(mustResolve(t, "account.oauth-onboarding-code"), "zh-Hans", "user@example.test", map[string]string{
		"code": "123456", "provider": "microsoft",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"哈利路亚家教会", "123456", "10 分钟"} {
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
	if !strings.Contains(email.Body, "https://account.alive.org.tw/reset-password?token=opaque") || email.HTMLBody == "" {
		t.Fatalf("RenderEmail() must include the reset URL in text and branded HTML: %#v", email)
	}
}

func TestRenderOAuthLinkConfirmation(t *testing.T) {
	definition, err := ResolveVersion("account.oauth-link-confirmation", 1, "email")
	if err != nil {
		t.Fatal(err)
	}
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
	definition, err := ResolveVersion("account.oauth-onboarding-code", 1, "email")
	if err != nil {
		t.Fatal(err)
	}
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
