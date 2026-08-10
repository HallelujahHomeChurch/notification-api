package templates

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
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

func TestRenderJapaneseAndKoreanAccountEmailTemplatesV3(t *testing.T) {
	verifyURL := "https://account.alive.org.tw/verify-email?token=opaque"
	resetURL := "https://account.alive.org.tw/reset-password?token=opaque"
	confirmURL := "https://account.alive.org.tw/oauth/link?token=opaque"
	for _, test := range []struct {
		name       string
		templateID string
		locale     string
		payload    map[string]string
		subject    string
		body       string
		htmlWants  []string
	}{
		{
			name: "Japanese email verification", templateID: "account.verify-email", locale: "ja",
			payload:   map[string]string{"verifyUrl": verifyURL},
			subject:   "HHCアカウントのメールアドレスを確認してください",
			body:      "HHCアカウントの作成ありがとうございます。下のボタンをクリックして、メールアドレスの確認を完了してください。このリンクは24時間後に期限切れになります。\n\nメールアドレスを確認: " + verifyURL + "\n\nこのHHCアカウントを作成していない場合は、このメールを無視してください。\n",
			htmlWants: []string{"ハレルヤ・ホーム・チャーチ", "メールアドレスを確認", `href="` + verifyURL + `"`},
		},
		{
			name: "Korean email verification", templateID: "account.verify-email", locale: "ko",
			payload:   map[string]string{"verifyUrl": verifyURL},
			subject:   "HHC 계정 이메일 주소를 확인해 주세요",
			body:      "HHC 계정을 만들어 주셔서 감사합니다. 아래 버튼을 눌러 이메일 주소 확인을 완료해 주세요. 이 링크는 24시간 후에 만료됩니다.\n\n이메일 주소 확인: " + verifyURL + "\n\nHHC 계정을 만든 적이 없다면 이 이메일을 무시하셔도 됩니다.\n",
			htmlWants: []string{"할렐루야 홈 교회", "이메일 주소 확인", `href="` + verifyURL + `"`},
		},
		{
			name: "Japanese password reset", templateID: "account.reset-password", locale: "ja",
			payload:   map[string]string{"resetUrl": resetURL},
			subject:   "HHCアカウントのパスワードをリセットしてください",
			body:      "パスワードをリセットするリクエストを受け付けました。下のボタンから新しいパスワードを設定してください。このリンクは1時間後に期限切れになります。\n\nパスワードをリセット: " + resetURL + "\n\nこのリクエストに心当たりがない場合は、このメールを無視してください。パスワードは変更されません。\n",
			htmlWants: []string{"パスワードをリセット", `href="` + resetURL + `"`},
		},
		{
			name: "Korean password reset", templateID: "account.reset-password", locale: "ko",
			payload:   map[string]string{"resetUrl": resetURL},
			subject:   "HHC 계정 비밀번호를 재설정해 주세요",
			body:      "비밀번호 재설정 요청을 받았습니다. 아래 버튼을 눌러 새 비밀번호를 설정해 주세요. 이 링크는 1시간 후에 만료됩니다.\n\n비밀번호 재설정: " + resetURL + "\n\n이 요청을 한 적이 없다면 이 이메일을 무시하셔도 됩니다. 비밀번호는 변경되지 않습니다.\n",
			htmlWants: []string{"비밀번호 재설정", `href="` + resetURL + `"`},
		},
		{
			name: "Japanese OAuth link confirmation", templateID: "account.oauth-link-confirmation", locale: "ja",
			payload:   map[string]string{"confirmUrl": confirmURL, "provider": "google"},
			subject:   "Googleログインの連携を確認してください",
			body:      "GoogleをHHCアカウントに連携することを確認してください。このリンクは15分後に期限切れになります。\n\n連携を確認: " + confirmURL + "\n\nこのログイン方法の連携を依頼していない場合は、このメールを無視してください。\n",
			htmlWants: []string{"Googleログインの連携を確認", "連携を確認", `href="` + confirmURL + `"`},
		},
		{
			name: "Korean OAuth link confirmation", templateID: "account.oauth-link-confirmation", locale: "ko",
			payload:   map[string]string{"confirmUrl": confirmURL, "provider": "google"},
			subject:   "Google 로그인 연결을 확인해 주세요",
			body:      "Google 로그인을 HHC 계정에 연결할지 확인해 주세요. 이 링크는 15분 후에 만료됩니다.\n\n연결 확인: " + confirmURL + "\n\n이 로그인 연결을 요청한 적이 없다면 이 이메일을 무시하셔도 됩니다.\n",
			htmlWants: []string{"Google 로그인 연결을 확인", "연결 확인", `href="` + confirmURL + `"`},
		},
		{
			name: "Japanese third-party email verification", templateID: "account.oauth-onboarding-code", locale: "ja",
			payload:   map[string]string{"code": "123456", "provider": "google"},
			subject:   "HHCアカウントのメールアドレスを確認してください",
			body:      "次の確認コードを使用して、Googleログイン用のメールアドレス確認を完了してください。\n\n123456\n\nこのコードは10分後に期限切れになります。 このコードを他人と共有しないでください。この操作に心当たりがない場合は、このメールを無視してください。\n",
			htmlWants: []string{"確認コードを入力", "123456", "このコードは10分後に期限切れになります。"},
		},
		{
			name: "Korean third-party email verification", templateID: "account.oauth-onboarding-code", locale: "ko",
			payload:   map[string]string{"code": "123456", "provider": "google"},
			subject:   "HHC 계정 이메일 주소를 확인해 주세요",
			body:      "아래 인증 코드를 사용하여 Google 로그인에 사용할 이메일 주소 확인을 완료해 주세요.\n\n123456\n\n이 코드는 10분 후에 만료됩니다. 이 코드를 다른 사람과 공유하지 마세요. 이 요청을 한 적이 없다면 이 이메일을 무시하셔도 됩니다.\n",
			htmlWants: []string{"인증 코드 입력", "123456", "이 코드는 10분 후에 만료됩니다."},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition, err := ResolveVersion(test.templateID, 3, "email")
			if err != nil {
				t.Fatal(err)
			}
			email, err := RenderEmail(definition, test.locale, "user@example.test", test.payload)
			if err != nil {
				t.Fatal(err)
			}
			if email.Subject != test.subject || email.Body != test.body {
				t.Fatalf("RenderEmail() = subject %q body %q, want subject %q body %q", email.Subject, email.Body, test.subject, test.body)
			}
			for _, want := range append(test.htmlWants, "font-family:-apple-system,BlinkMacSystemFont,Segoe UI,sans-serif") {
				if !strings.Contains(email.HTMLBody, want) {
					t.Fatalf("HTML body missing %q", want)
				}
			}
		})
	}
}

func TestRenderJapaneseAndKoreanNewsletterWrapperV3(t *testing.T) {
	payload := map[string]string{
		"subject": "August news", "body": "Church updates", "actionUrl": "https://www.alive.org.tw/en/news",
		"unsubscribeUrl":         "https://www.alive.org.tw/en/newsletter/unsubscribe?token=opaque",
		"oneClickUnsubscribeUrl": "https://www.alive.org.tw/api/engagement/v1/newsletter/unsubscribe?token=opaque",
	}
	for _, test := range []struct {
		locale      string
		church      string
		readMore    string
		unsubscribe string
	}{
		{locale: "ja", church: "ハレルヤ・ホーム・チャーチ", readMore: "詳しく見る", unsubscribe: "配信停止"},
		{locale: "ko", church: "할렐루야 홈 교회", readMore: "자세히 보기", unsubscribe: "구독 취소"},
	} {
		t.Run(test.locale, func(t *testing.T) {
			definition, err := ResolveVersion("engagement.newsletter", 3, "email")
			if err != nil {
				t.Fatal(err)
			}
			email, err := RenderEmail(definition, test.locale, "user@example.test", payload)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{test.church, test.readMore, test.unsubscribe, `lang="` + test.locale + `"`, "font-family:-apple-system,BlinkMacSystemFont,Segoe UI,sans-serif"} {
				if !strings.Contains(email.HTMLBody, want) {
					t.Fatalf("HTML body missing %q", want)
				}
			}
			if !strings.Contains(email.Body, test.unsubscribe+": "+payload["unsubscribeUrl"]) {
				t.Fatalf("plain text body missing localized unsubscribe copy: %q", email.Body)
			}
		})
	}
}

func TestRenderJapaneseAndKoreanWebPushV3PreservesCallerCopy(t *testing.T) {
	definition, err := ResolveVersion("engagement.web-push", 3, "web_push")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ locale, title, body string }{
		{locale: "ja", title: "教会からのお知らせ", body: "最新のお知らせをご確認ください。"},
		{locale: "ko", title: "교회 소식", body: "새 소식을 확인해 주세요."},
	} {
		t.Run(test.locale, func(t *testing.T) {
			push, err := RenderWebPush(definition, test.locale, "subscription", map[string]string{
				"title": test.title, "body": test.body, "clickBehavior": "home",
			})
			if err != nil {
				t.Fatal(err)
			}
			if push.Title != test.title || push.Body != test.body || push.ClickBehavior != "home" {
				t.Fatalf("RenderWebPush() = %#v", push)
			}
		})
	}
}

func TestHistoricalQueuedVersionTwoOutputRemainsStable(t *testing.T) {
	email, err := RenderEmail(mustResolve(t, "account.verify-email"), "zh-Hant", "user@example.test", map[string]string{
		"verifyUrl": "https://account.alive.org.tw/verify-email?token=queued-v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprintf("%x", sha256.Sum256([]byte(email.Subject+"\x00"+email.Body+"\x00"+email.HTMLBody))), "2243639db3f5a2b1f30c161e393f26b9c5b29c82d26fcacd1635a6a74db31382"; got != want {
		t.Fatalf("queued v2 output hash = %s, want %s", got, want)
	}
}

func TestV3EmailNormalTextColorsMeetWCAGAA(t *testing.T) {
	action, err := RenderEmail(mustResolveVersion(t, "account.verify-email", 3), "ja", "user@example.test", map[string]string{
		"verifyUrl": "https://account.alive.org.tw/verify-email?token=opaque",
	})
	if err != nil {
		t.Fatal(err)
	}
	code, err := RenderEmail(mustResolveVersion(t, "account.oauth-onboarding-code", 3), "ko", "user@example.test", map[string]string{
		"code": "123456", "provider": "line",
	})
	if err != nil {
		t.Fatal(err)
	}
	newsletter, err := RenderEmail(mustResolveVersion(t, "engagement.newsletter", 3), "ja", "user@example.test", map[string]string{
		"subject": "August news", "body": "Church updates", "actionUrl": "https://www.alive.org.tw/ja/news",
		"unsubscribeUrl":         "https://www.alive.org.tw/ja/newsletter/unsubscribe?token=opaque",
		"oneClickUnsubscribeUrl": "https://www.alive.org.tw/api/engagement/v1/newsletter/unsubscribe?token=opaque",
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, email := range map[string]Email{"action": action, "code": code, "newsletter": newsletter} {
		t.Run(name+" footer", func(t *testing.T) {
			assertHTMLContrast(t, email.HTMLBody, `border-top:[^;]+;color:(#[0-9a-f]{6});font-size:13px`, "#fffdf9")
		})
	}
	for name, email := range map[string]Email{"action": action, "newsletter": newsletter} {
		t.Run(name+" CTA", func(t *testing.T) {
			match := regexp.MustCompile(`background:(#[0-9a-f]{6});color:(#[0-9a-f]{6});text-decoration`).FindStringSubmatch(email.HTMLBody)
			if len(match) != 3 {
				t.Fatal("CTA colors not found")
			}
			assertContrast(t, match[2], match[1])
		})
	}
}

func assertHTMLContrast(t *testing.T, htmlBody, pattern, background string) {
	t.Helper()
	match := regexp.MustCompile(pattern).FindStringSubmatch(htmlBody)
	if len(match) != 2 {
		t.Fatal("text color not found")
	}
	assertContrast(t, match[1], background)
}

func assertContrast(t *testing.T, foreground, background string) {
	t.Helper()
	ratio := contrastRatio(foreground, background)
	t.Logf("contrast %s on %s = %.4f:1", foreground, background, ratio)
	if ratio < 4.5 {
		t.Fatalf("contrast %s on %s = %.2f:1, want at least 4.5:1", foreground, background, ratio)
	}
}

func contrastRatio(first, second string) float64 {
	firstLuminance, secondLuminance := relativeLuminance(first), relativeLuminance(second)
	return (math.Max(firstLuminance, secondLuminance) + 0.05) / (math.Min(firstLuminance, secondLuminance) + 0.05)
}

func relativeLuminance(color string) float64 {
	channels := make([]float64, 3)
	for index := range channels {
		value, err := strconv.ParseUint(color[1+index*2:3+index*2], 16, 8)
		if err != nil {
			panic(err)
		}
		channel := float64(value) / 255
		if channel <= 0.04045 {
			channels[index] = channel / 12.92
		} else {
			channels[index] = math.Pow((channel+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*channels[0] + 0.7152*channels[1] + 0.0722*channels[2]
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

func TestRenderWebPushPayload(t *testing.T) {
	push, err := RenderWebPush(
		mustResolveChannel(t, "engagement.web-push", "web_push"),
		"zh-Hant",
		`{"endpoint":"https://push.example.test/subscription","keys":{"p256dh":"BGsX0fLhLEJH-Lzm5WOkQPJ3A32BLeszoPShOUXYmMKWT-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU","auth":"AAAAAAAAAAAAAAAAAAAAAA"}}`,
		map[string]string{
			"title":         "八月消息",
			"body":          "教會近況",
			"clickBehavior": "url",
			"actionUrl":     "https://www.alive.org.tw/zh-Hant/news",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if push.Target == "" || push.Title != "八月消息" || push.Body != "教會近況" ||
		push.ClickBehavior != "url" || push.ActionURL != "https://www.alive.org.tw/zh-Hant/news" {
		t.Fatalf("RenderWebPush() = %#v", push)
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
	v99 := cloneDefinition(v1)
	v99.Version = 99
	v99.RequiredFields = set("confirmationUrl")
	v99.AllowedFields = set("confirmationUrl")
	definitions[templateID] = map[int]Definition{1: v1, 99: v99}
	currentVersions[templateID] = 99

	current, err := Resolve(templateID, "email")
	if err != nil || current.Version != 99 {
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
	if _, err := RenderEmail(v99, "en", "user@example.test", map[string]string{
		"confirmationUrl": "https://account.alive.org.tw/verify-email?token=v99",
	}); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("RenderEmail(unimplemented v99) error = %v, want ErrUnknownTemplate", err)
	}
}

func mustResolveChannel(t *testing.T, templateID, channel string) Definition {
	t.Helper()
	definition, err := Resolve(templateID, channel)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func mustResolveVersion(t *testing.T, templateID string, version int) Definition {
	t.Helper()
	definition, err := ResolveVersion(templateID, version, "email")
	if err != nil {
		t.Fatal(err)
	}
	return definition
}
