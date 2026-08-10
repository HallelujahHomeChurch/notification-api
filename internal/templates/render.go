package templates

import (
	"fmt"
	"html"
	"strings"
)

type Email struct {
	To                  string
	Subject             string
	Body                string
	HTMLBody            string
	ListUnsubscribe     string
	OneClickUnsubscribe bool
}

type WebPush struct {
	Target        string
	Title         string
	Body          string
	ClickBehavior string
	ActionURL     string
}

func RenderWebPush(definition Definition, locale, target string, payload map[string]string) (WebPush, error) {
	canonical, err := ResolveVersion(definition.ID, definition.Version, definition.Channel)
	if err != nil {
		return WebPush{}, err
	}
	validated, err := validatePayload(canonical, payload)
	if err != nil {
		return WebPush{}, err
	}
	if !canonical.SupportedLocale[locale] {
		locale = "en"
	}
	clickBehavior := validated["clickBehavior"]
	if clickBehavior == "" {
		if validated["actionUrl"] == "" {
			clickBehavior = "home"
		} else {
			clickBehavior = "url"
		}
	}
	return WebPush{
		Target: target, Title: validated["title"], Body: validated["body"],
		ClickBehavior: clickBehavior, ActionURL: validated["actionUrl"],
	}, nil
}

func RenderEmail(definition Definition, locale, to string, payload map[string]string) (Email, error) {
	canonical, err := ResolveVersion(definition.ID, definition.Version, definition.Channel)
	if err != nil {
		return Email{}, err
	}
	validated, err := validatePayload(canonical, payload)
	if err != nil {
		return Email{}, err
	}
	if !canonical.SupportedLocale[locale] {
		locale = "en"
	}

	switch {
	case canonical.ID == "account.verify-email" && canonical.Version == 1:
		return renderVerificationEmailV1(locale, to, validated["verifyUrl"]), nil
	case canonical.ID == "account.verify-email" && canonical.Version == 2:
		return renderVerificationEmail(locale, to, validated["verifyUrl"]), nil
	case canonical.ID == "account.verify-email" && canonical.Version == 3:
		return renderVerificationEmailV3(locale, to, validated["verifyUrl"]), nil
	case canonical.ID == "account.reset-password" && canonical.Version == 1:
		return renderPasswordResetEmailV1(locale, to, validated["resetUrl"]), nil
	case canonical.ID == "account.reset-password" && canonical.Version == 2:
		return renderPasswordResetEmail(locale, to, validated["resetUrl"]), nil
	case canonical.ID == "account.reset-password" && canonical.Version == 3:
		return renderPasswordResetEmailV3(locale, to, validated["resetUrl"]), nil
	case canonical.ID == "account.oauth-link-confirmation" && canonical.Version == 1:
		return renderOAuthLinkConfirmationEmailV1(locale, to, validated["confirmUrl"], validated["provider"]), nil
	case canonical.ID == "account.oauth-link-confirmation" && canonical.Version == 2:
		return renderOAuthLinkConfirmationEmail(locale, to, validated["confirmUrl"], validated["provider"]), nil
	case canonical.ID == "account.oauth-link-confirmation" && canonical.Version == 3:
		return renderOAuthLinkConfirmationEmailV3(locale, to, validated["confirmUrl"], validated["provider"]), nil
	case canonical.ID == "account.oauth-onboarding-code" && canonical.Version == 1:
		return renderOAuthOnboardingCodeEmailV1(locale, to, validated["code"]), nil
	case canonical.ID == "account.oauth-onboarding-code" && canonical.Version == 2:
		return renderOAuthOnboardingCodeEmail(locale, to, validated["code"], validated["provider"]), nil
	case canonical.ID == "account.oauth-onboarding-code" && canonical.Version == 3:
		return renderOAuthOnboardingCodeEmailV3(locale, to, validated["code"], validated["provider"]), nil
	case canonical.ID == "engagement.newsletter" && canonical.Version == 1:
		return renderNewsletterEmail(locale, to, validated), nil
	case canonical.ID == "engagement.newsletter" && canonical.Version == 2:
		return renderNewsletterEmail(locale, to, validated), nil
	case canonical.ID == "engagement.newsletter" && canonical.Version == 3:
		return renderNewsletterEmailV3(locale, to, validated), nil
	default:
		return Email{}, fmt.Errorf(
			"%w: %s version %d",
			ErrUnknownTemplate,
			canonical.ID,
			canonical.Version,
		)
	}
}

func renderVerificationEmailV3(locale, to, verifyURL string) Email {
	if locale != "ja" && locale != "ko" {
		return systemFontEmail(renderVerificationEmail(locale, to, verifyURL))
	}
	var subject, church, heading, message, action, footer string
	if locale == "ja" {
		subject, church, heading = "HHCアカウントのメールアドレスを確認してください", "ハレルヤ・ホーム・チャーチ", "メールアドレスを確認"
		message, action = "HHCアカウントの作成ありがとうございます。下のボタンをクリックして、メールアドレスの確認を完了してください。このリンクは24時間後に期限切れになります。", "メールアドレスを確認"
		footer = "このHHCアカウントを作成していない場合は、このメールを無視してください。"
	} else {
		subject, church, heading = "HHC 계정 이메일 주소를 확인해 주세요", "할렐루야 홈 교회", "이메일 주소 확인"
		message, action = "HHC 계정을 만들어 주셔서 감사합니다. 아래 버튼을 눌러 이메일 주소 확인을 완료해 주세요. 이 링크는 24시간 후에 만료됩니다.", "이메일 주소 확인"
		footer = "HHC 계정을 만든 적이 없다면 이 이메일을 무시하셔도 됩니다."
	}
	body := message + "\n\n" + action + ": " + verifyURL + "\n\n" + footer + "\n"
	return Email{To: to, Subject: subject, Body: body, HTMLBody: brandedEmailHTMLV3(locale, church, heading, message, action, verifyURL, footer)}
}

func renderPasswordResetEmailV3(locale, to, resetURL string) Email {
	if locale != "ja" && locale != "ko" {
		return systemFontEmail(renderPasswordResetEmail(locale, to, resetURL))
	}
	var subject, church, heading, message, action, footer string
	if locale == "ja" {
		subject, church, heading = "HHCアカウントのパスワードをリセットしてください", "ハレルヤ・ホーム・チャーチ", "パスワードをリセット"
		message, action = "パスワードをリセットするリクエストを受け付けました。下のボタンから新しいパスワードを設定してください。このリンクは1時間後に期限切れになります。", "パスワードをリセット"
		footer = "このリクエストに心当たりがない場合は、このメールを無視してください。パスワードは変更されません。"
	} else {
		subject, church, heading = "HHC 계정 비밀번호를 재설정해 주세요", "할렐루야 홈 교회", "비밀번호 재설정"
		message, action = "비밀번호 재설정 요청을 받았습니다. 아래 버튼을 눌러 새 비밀번호를 설정해 주세요. 이 링크는 1시간 후에 만료됩니다.", "비밀번호 재설정"
		footer = "이 요청을 한 적이 없다면 이 이메일을 무시하셔도 됩니다. 비밀번호는 변경되지 않습니다."
	}
	body := message + "\n\n" + action + ": " + resetURL + "\n\n" + footer + "\n"
	return Email{To: to, Subject: subject, Body: body, HTMLBody: brandedEmailHTMLV3(locale, church, heading, message, action, resetURL, footer)}
}

func renderOAuthLinkConfirmationEmailV3(locale, to, confirmURL, provider string) Email {
	if locale != "ja" && locale != "ko" {
		return systemFontEmail(renderOAuthLinkConfirmationEmail(locale, to, confirmURL, provider))
	}
	providerName := map[string]string{"google": "Google", "line": "LINE", "microsoft": "Microsoft"}[provider]
	var subject, church, heading, message, action, footer string
	if locale == "ja" {
		subject, church, heading = providerName+"ログインの連携を確認してください", "ハレルヤ・ホーム・チャーチ", providerName+"ログインの連携を確認"
		message, action = providerName+"をHHCアカウントに連携することを確認してください。このリンクは15分後に期限切れになります。", "連携を確認"
		footer = "このログイン方法の連携を依頼していない場合は、このメールを無視してください。"
	} else {
		subject, church, heading = providerName+" 로그인 연결을 확인해 주세요", "할렐루야 홈 교회", providerName+" 로그인 연결을 확인"
		message, action = providerName+" 로그인을 HHC 계정에 연결할지 확인해 주세요. 이 링크는 15분 후에 만료됩니다.", "연결 확인"
		footer = "이 로그인 연결을 요청한 적이 없다면 이 이메일을 무시하셔도 됩니다."
	}
	body := message + "\n\n" + action + ": " + confirmURL + "\n\n" + footer + "\n"
	return Email{To: to, Subject: subject, Body: body, HTMLBody: brandedEmailHTMLV3(locale, church, heading, message, action, confirmURL, footer)}
}

func renderOAuthOnboardingCodeEmailV3(locale, to, code, provider string) Email {
	if locale != "ja" && locale != "ko" {
		return systemFontEmail(renderOAuthOnboardingCodeEmail(locale, to, code, provider))
	}
	providerName := map[string]string{"google": "Google", "line": "LINE", "microsoft": "Microsoft"}[provider]
	var subject, church, heading, message, expiry, footer string
	if locale == "ja" {
		subject, church, heading = "HHCアカウントのメールアドレスを確認してください", "ハレルヤ・ホーム・チャーチ", "確認コードを入力"
		message, expiry = "次の確認コードを使用して、"+providerName+"ログイン用のメールアドレス確認を完了してください。", "このコードは10分後に期限切れになります。"
		footer = "このコードを他人と共有しないでください。この操作に心当たりがない場合は、このメールを無視してください。"
	} else {
		subject, church, heading = "HHC 계정 이메일 주소를 확인해 주세요", "할렐루야 홈 교회", "인증 코드 입력"
		message, expiry = "아래 인증 코드를 사용하여 "+providerName+" 로그인에 사용할 이메일 주소 확인을 완료해 주세요.", "이 코드는 10분 후에 만료됩니다."
		footer = "이 코드를 다른 사람과 공유하지 마세요. 이 요청을 한 적이 없다면 이 이메일을 무시하셔도 됩니다."
	}
	body := message + "\n\n" + code + "\n\n" + expiry + " " + footer + "\n"
	return Email{To: to, Subject: subject, Body: body, HTMLBody: brandedCodeEmailHTMLV3(locale, church, heading, message, code, expiry, footer)}
}

func renderNewsletterEmail(locale, to string, payload map[string]string) Email {
	church, unsubscribe, readMore := "Hallelujah Home Church", "Unsubscribe", "Read more"
	if locale == "zh-Hant" {
		church, unsubscribe, readMore = "哈利路亞家教會", "取消訂閱", "閱讀更多"
	}
	if locale == "zh-Hans" {
		church, unsubscribe, readMore = "哈利路亚家教会", "取消订阅", "阅读更多"
	}
	action := ""
	if payload["actionUrl"] != "" {
		action = fmt.Sprintf(`<p style="margin:28px 0"><a href="%s" style="display:inline-block;background:#c75d55;color:#fffaf5;text-decoration:none;font-weight:700;padding:13px 22px;border-radius:6px">%s</a></p>`, html.EscapeString(payload["actionUrl"]), html.EscapeString(readMore))
	}
	bodyHTML := strings.ReplaceAll(html.EscapeString(payload["body"]), "\n", "<br>")
	htmlBody := fmt.Sprintf(`<!doctype html><html lang="%s"><body style="margin:0;background:#fbf5eb;color:#342d2b;font-family:Arial,'Noto Sans TC',sans-serif"><table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="background:#fbf5eb;padding:32px 16px"><tr><td align="center"><table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="max-width:600px;background:#fffdf9;border:1px solid #eaded2;border-radius:8px"><tr><td style="padding:32px"><p style="margin:0 0 28px;color:#b94f47;font-size:16px;font-weight:700">%s</p><h1 style="margin:0 0 20px;font-size:26px;line-height:1.35">%s</h1><p style="margin:0;color:#665c58;font-size:16px;line-height:1.75">%s</p>%s<p style="margin:32px 0 0;padding-top:20px;border-top:1px solid #eaded2;color:#827773;font-size:13px"><a href="%s" style="color:#827773">%s</a></p></td></tr></table></td></tr></table></body></html>`, html.EscapeString(locale), html.EscapeString(church), html.EscapeString(payload["subject"]), bodyHTML, action, html.EscapeString(payload["unsubscribeUrl"]), html.EscapeString(unsubscribe))
	body := payload["body"]
	if payload["actionUrl"] != "" {
		body += "\n\n" + payload["actionUrl"]
	}
	body += "\n\n" + unsubscribe + ": " + payload["unsubscribeUrl"] + "\n"
	oneClickURL := payload["oneClickUnsubscribeUrl"]
	if oneClickURL == "" {
		oneClickURL = payload["unsubscribeUrl"]
	}
	return Email{To: to, Subject: payload["subject"], Body: body, HTMLBody: htmlBody, ListUnsubscribe: "<" + oneClickURL + ">", OneClickUnsubscribe: true}
}

func renderNewsletterEmailV3(locale, to string, payload map[string]string) Email {
	if locale != "ja" && locale != "ko" {
		return systemFontEmail(renderNewsletterEmail(locale, to, payload))
	}
	church, unsubscribe, readMore := "ハレルヤ・ホーム・チャーチ", "配信停止", "詳しく見る"
	if locale == "ko" {
		church, unsubscribe, readMore = "할렐루야 홈 교회", "구독 취소", "자세히 보기"
	}
	action := ""
	if payload["actionUrl"] != "" {
		action = fmt.Sprintf(`<p style="margin:28px 0"><a href="%s" style="display:inline-block;background:#c75d55;color:#fffaf5;text-decoration:none;font-weight:700;padding:13px 22px;border-radius:6px">%s</a></p>`, html.EscapeString(payload["actionUrl"]), html.EscapeString(readMore))
	}
	bodyHTML := strings.ReplaceAll(html.EscapeString(payload["body"]), "\n", "<br>")
	htmlBody := fmt.Sprintf(`<!doctype html><html lang="%s"><body style="margin:0;background:#fbf5eb;color:#342d2b;font-family:-apple-system,BlinkMacSystemFont,Segoe UI,sans-serif"><table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="background:#fbf5eb;padding:32px 16px"><tr><td align="center"><table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="max-width:600px;background:#fffdf9;border:1px solid #eaded2;border-radius:8px"><tr><td style="padding:32px"><p style="margin:0 0 28px;color:#b94f47;font-size:16px;font-weight:700">%s</p><h1 style="margin:0 0 20px;font-size:26px;line-height:1.35">%s</h1><p style="margin:0;color:#665c58;font-size:16px;line-height:1.75">%s</p>%s<p style="margin:32px 0 0;padding-top:20px;border-top:1px solid #eaded2;color:#827773;font-size:13px"><a href="%s" style="color:#827773">%s</a></p></td></tr></table></td></tr></table></body></html>`, html.EscapeString(locale), html.EscapeString(church), html.EscapeString(payload["subject"]), bodyHTML, action, html.EscapeString(payload["unsubscribeUrl"]), html.EscapeString(unsubscribe))
	body := payload["body"]
	if payload["actionUrl"] != "" {
		body += "\n\n" + payload["actionUrl"]
	}
	body += "\n\n" + unsubscribe + ": " + payload["unsubscribeUrl"] + "\n"
	return Email{To: to, Subject: payload["subject"], Body: body, HTMLBody: systemFontHTML(htmlBody), ListUnsubscribe: "<" + payload["oneClickUnsubscribeUrl"] + ">", OneClickUnsubscribe: true}
}

func renderOAuthOnboardingCodeEmailV1(locale, to, code string) Email {
	switch locale {
	case "zh-Hant":
		return Email{To: to, Subject: "驗證您的 HHC 帳戶 Email", Body: "您的 HHC 帳戶驗證碼是：" + code + "\n\n驗證碼將於 10 分鐘後失效。請勿將驗證碼提供給他人。\n"}
	case "zh-Hans":
		return Email{To: to, Subject: "验证您的 HHC 帐户 Email", Body: "您的 HHC 帐户验证码是：" + code + "\n\n验证码将在 10 分钟后失效。请勿将验证码提供给他人。\n"}
	default:
		return Email{To: to, Subject: "Verify your HHC account email", Body: "Your HHC account verification code is: " + code + "\n\nThe code expires in 10 minutes. Do not share it with anyone.\n"}
	}
}

func renderOAuthLinkConfirmationEmailV1(locale, to, confirmURL, provider string) Email {
	providerName := map[string]string{
		"google":    "Google",
		"line":      "LINE",
		"microsoft": "Microsoft",
	}[provider]
	switch locale {
	case "zh-Hant":
		return Email{To: to, Subject: "確認連結 " + providerName + " 登入", Body: "如果您剛剛要求將 " + providerName + " 連結到 HHC 帳戶，請使用以下連結確認：\n\n" + confirmURL + "\n"}
	case "zh-Hans":
		return Email{To: to, Subject: "确认关联 " + providerName + " 登录", Body: "如果您刚刚要求将 " + providerName + " 关联到 HHC 帐户，请使用以下链接确认：\n\n" + confirmURL + "\n"}
	default:
		return Email{To: to, Subject: "Confirm " + providerName + " sign-in link", Body: "If you requested to link " + providerName + " to your HHC account, confirm it using this link:\n\n" + confirmURL + "\n"}
	}
}

func renderOAuthLinkConfirmationEmail(locale, to, confirmURL, provider string) Email {
	providerName := map[string]string{"google": "Google", "line": "LINE", "microsoft": "Microsoft"}[provider]
	var subject, church, heading, message, action, footer string
	switch locale {
	case "zh-Hant":
		subject, church, heading = "確認連結 "+providerName+" 登入", "哈利路亞家教會", "確認 "+providerName+" 登入"
		message, action = "請確認將 "+providerName+" 連結至您的 HHC 帳戶。此連結將於 15 分鐘後失效。", "確認連結"
		footer = "如果您沒有要求連結此登入方式，請忽略這封信。"
	case "zh-Hans":
		subject, church, heading = "确认关联 "+providerName+" 登录", "哈利路亚家教会", "确认 "+providerName+" 登录"
		message, action = "请确认将 "+providerName+" 关联至您的 HHC 帐户。此链接将在 15 分钟后失效。", "确认关联"
		footer = "如果您没有要求关联此登录方式，请忽略这封邮件。"
	default:
		subject, church, heading = "Confirm "+providerName+" sign-in link", "Hallelujah Home Church", "Confirm "+providerName+" sign-in"
		message, action = "Confirm that you want to link "+providerName+" to your HHC Account. This link expires in 15 minutes.", "Confirm link"
		footer = "If you did not request this sign-in link, you can ignore this email."
	}
	body := message + "\n\n" + action + ": " + confirmURL + "\n\n" + footer + "\n"
	return Email{To: to, Subject: subject, Body: body, HTMLBody: brandedEmailHTML(locale, church, heading, message, action, confirmURL, footer)}
}

func renderOAuthOnboardingCodeEmail(locale, to, code, provider string) Email {
	providerName := map[string]string{"google": "Google", "line": "LINE", "microsoft": "Microsoft"}[provider]
	var subject, church, heading, message, expiry, footer string
	switch locale {
	case "zh-Hant":
		subject, church, heading = "驗證您的 HHC 帳戶 Email", "哈利路亞家教會", "輸入驗證碼"
		message, expiry = "使用以下驗證碼完成 "+providerName+" 登入的 Email 驗證。", "驗證碼將於 10 分鐘後失效。"
		footer = "請勿將驗證碼提供給他人。如果您沒有進行此操作，可以忽略這封信。"
	case "zh-Hans":
		subject, church, heading = "验证您的 HHC 帐户 Email", "哈利路亚家教会", "输入验证码"
		message, expiry = "使用以下验证码完成 "+providerName+" 登录的 Email 验证。", "验证码将在 10 分钟后失效。"
		footer = "请勿将验证码提供给他人。如果您没有进行此操作，可以忽略这封邮件。"
	default:
		subject, church, heading = "Verify your HHC account email", "Hallelujah Home Church", "Enter verification code"
		message, expiry = "Use this code to verify the email for your "+providerName+" sign-in.", "The code expires in 10 minutes."
		footer = "Do not share this code. If you did not make this request, you can ignore this email."
	}
	body := message + "\n\n" + code + "\n\n" + expiry + " " + footer + "\n"
	return Email{To: to, Subject: subject, Body: body, HTMLBody: brandedCodeEmailHTML(locale, church, heading, message, code, expiry, footer)}
}

func renderVerificationEmailV1(locale, to, verifyURL string) Email {
	switch locale {
	case "zh-Hant":
		return Email{To: to, Subject: "驗證您的 HHC 帳戶", Body: "請使用以下連結驗證您的 HHC 帳戶：\n\n" + verifyURL + "\n"}
	case "zh-Hans":
		return Email{To: to, Subject: "验证您的 HHC 帐户", Body: "请使用以下链接验证您的 HHC 帐户：\n\n" + verifyURL + "\n"}
	default:
		return Email{To: to, Subject: "Verify your HHC account", Body: "Use this link to verify your HHC account:\n\n" + verifyURL + "\n"}
	}
}

func renderVerificationEmail(locale, to, verifyURL string) Email {
	var subject, church, heading, message, action, footer string
	switch locale {
	case "zh-Hant":
		subject, church, heading = "驗證您的 Email", "哈利路亞家教會", "驗證您的 Email"
		message, action = "感謝您建立 HHC 帳戶。請點選下方按鈕完成 Email 驗證。此連結將於 24 小時後失效。", "驗證 Email"
		footer = "如果您沒有建立 HHC 帳戶，可以忽略這封信。"
	case "zh-Hans":
		subject, church, heading = "验证您的 Email", "哈利路亚家教会", "验证您的 Email"
		message, action = "感谢您建立 HHC 帐户。请点击下方按钮完成 Email 验证。此链接将在 24 小时后失效。", "验证 Email"
		footer = "如果您没有建立 HHC 帐户，可以忽略这封邮件。"
	default:
		subject, church, heading = "Verify your email", "Hallelujah Home Church", "Verify your email"
		message, action = "Thank you for creating an HHC Account. Select the button below to verify your email. This link expires in 24 hours.", "Verify email"
		footer = "If you did not create an HHC Account, you can ignore this email."
	}
	body := message + "\n\n" + action + ": " + verifyURL + "\n\n" + footer + "\n"
	return Email{
		To: to, Subject: subject, Body: body,
		HTMLBody: brandedEmailHTML(locale, church, heading, message, action, verifyURL, footer),
	}
}

func brandedEmailHTML(locale, church, heading, message, action, actionURL, footer string) string {
	return fmt.Sprintf(`<!doctype html><html lang="%s"><body style="margin:0;background:#fbf5eb;color:#342d2b;font-family:Arial,'Noto Sans TC',sans-serif"><table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="background:#fbf5eb;padding:32px 16px"><tr><td align="center"><table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="max-width:560px;background:#fffdf9;border:1px solid #eaded2;border-radius:8px"><tr><td style="padding:32px"><p style="margin:0 0 28px;color:#b94f47;font-size:16px;font-weight:700">%s</p><h1 style="margin:0 0 16px;font-size:26px;line-height:1.35">%s</h1><p style="margin:0 0 28px;color:#665c58;font-size:16px;line-height:1.75">%s</p><a href="%s" style="display:inline-block;background:#c75d55;color:#fffaf5;text-decoration:none;font-weight:700;padding:13px 22px;border-radius:6px">%s</a><p style="margin:32px 0 0;padding-top:20px;border-top:1px solid #eaded2;color:#827773;font-size:13px;line-height:1.65">%s</p></td></tr></table></td></tr></table></body></html>`,
		html.EscapeString(locale), html.EscapeString(church), html.EscapeString(heading), html.EscapeString(message),
		html.EscapeString(actionURL), html.EscapeString(action), html.EscapeString(footer))
}

func brandedEmailHTMLV3(locale, church, heading, message, action, actionURL, footer string) string {
	return systemFontHTML(brandedEmailHTML(locale, church, heading, message, action, actionURL, footer))
}

func brandedCodeEmailHTML(locale, church, heading, message, code, expiry, footer string) string {
	return fmt.Sprintf(`<!doctype html><html lang="%s"><body style="margin:0;background:#fbf5eb;color:#342d2b;font-family:Arial,'Noto Sans TC',sans-serif"><table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="background:#fbf5eb;padding:32px 16px"><tr><td align="center"><table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="max-width:560px;background:#fffdf9;border:1px solid #eaded2;border-radius:8px"><tr><td style="padding:32px"><p style="margin:0 0 28px;color:#b94f47;font-size:16px;font-weight:700">%s</p><h1 style="margin:0 0 16px;font-size:26px;line-height:1.35">%s</h1><p style="margin:0 0 24px;color:#665c58;font-size:16px;line-height:1.75">%s</p><p style="margin:0 0 20px;padding:18px;background:#f7ebe5;border-radius:6px;text-align:center;font-size:32px;font-weight:700;letter-spacing:8px">%s</p><p style="margin:0;color:#665c58;font-size:14px;line-height:1.65">%s</p><p style="margin:32px 0 0;padding-top:20px;border-top:1px solid #eaded2;color:#827773;font-size:13px;line-height:1.65">%s</p></td></tr></table></td></tr></table></body></html>`,
		html.EscapeString(locale), html.EscapeString(church), html.EscapeString(heading), html.EscapeString(message),
		html.EscapeString(code), html.EscapeString(expiry), html.EscapeString(footer))
}

func brandedCodeEmailHTMLV3(locale, church, heading, message, code, expiry, footer string) string {
	return systemFontHTML(brandedCodeEmailHTML(locale, church, heading, message, code, expiry, footer))
}

func systemFontEmail(email Email) Email {
	email.HTMLBody = systemFontHTML(email.HTMLBody)
	return email
}

func systemFontHTML(body string) string {
	return strings.NewReplacer(
		"font-family:Arial,'Noto Sans TC',sans-serif", "font-family:-apple-system,BlinkMacSystemFont,Segoe UI,sans-serif",
		"background:#c75d55;color:#fffaf5", "background:#b94f47;color:#fffaf5",
		"color:#827773", "color:#7e736f",
	).Replace(body)
}

func renderPasswordResetEmailV1(locale, to, resetURL string) Email {
	switch locale {
	case "zh-Hant":
		return Email{To: to, Subject: "重設您的 HHC 帳戶密碼", Body: "請使用以下連結重設您的 HHC 帳戶密碼：\n\n" + resetURL + "\n"}
	case "zh-Hans":
		return Email{To: to, Subject: "重置您的 HHC 帐户密码", Body: "请使用以下链接重置您的 HHC 帐户密码：\n\n" + resetURL + "\n"}
	default:
		return Email{To: to, Subject: "Reset your HHC account password", Body: "Use this link to reset your HHC account password:\n\n" + resetURL + "\n"}
	}
}

func renderPasswordResetEmail(locale, to, resetURL string) Email {
	var subject, church, heading, message, action, footer string
	switch locale {
	case "zh-Hant":
		subject, church, heading = "重設您的 HHC 帳戶密碼", "哈利路亞家教會", "重設密碼"
		message, action = "我們收到您的密碼重設要求。請點選下方按鈕設定新密碼。此連結將於 1 小時後失效。", "重設密碼"
		footer = "如果您沒有要求重設密碼，可以忽略這封信；您的密碼不會被變更。"
	case "zh-Hans":
		subject, church, heading = "重置您的 HHC 帐户密码", "哈利路亚家教会", "重置密码"
		message, action = "我们收到了您的密码重置请求。请点击下方按钮设置新密码。此链接将在 1 小时后失效。", "重置密码"
		footer = "如果您没有要求重置密码，可以忽略这封邮件；您的密码不会被更改。"
	default:
		subject, church, heading = "Reset your HHC account password", "Hallelujah Home Church", "Reset password"
		message, action = "We received a request to reset your password. Select the button below to set a new password. This link expires in 1 hour.", "Reset password"
		footer = "If you did not request a password reset, you can ignore this email. Your password will not change."
	}
	body := message + "\n\n" + action + ": " + resetURL + "\n\n" + footer + "\n"
	return Email{To: to, Subject: subject, Body: body, HTMLBody: brandedEmailHTML(locale, church, heading, message, action, resetURL, footer)}
}
