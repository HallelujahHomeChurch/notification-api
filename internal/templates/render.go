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
	case canonical.ID == "account.reset-password" && canonical.Version == 1:
		return renderPasswordResetEmailV1(locale, to, validated["resetUrl"]), nil
	case canonical.ID == "account.reset-password" && canonical.Version == 2:
		return renderPasswordResetEmail(locale, to, validated["resetUrl"]), nil
	case canonical.ID == "account.oauth-link-confirmation" && canonical.Version == 1:
		return renderOAuthLinkConfirmationEmailV1(locale, to, validated["confirmUrl"], validated["provider"]), nil
	case canonical.ID == "account.oauth-link-confirmation" && canonical.Version == 2:
		return renderOAuthLinkConfirmationEmail(locale, to, validated["confirmUrl"], validated["provider"]), nil
	case canonical.ID == "account.oauth-onboarding-code" && canonical.Version == 1:
		return renderOAuthOnboardingCodeEmailV1(locale, to, validated["code"]), nil
	case canonical.ID == "account.oauth-onboarding-code" && canonical.Version == 2:
		return renderOAuthOnboardingCodeEmail(locale, to, validated["code"], validated["provider"]), nil
	case canonical.ID == "engagement.newsletter" && canonical.Version == 1:
		return renderNewsletterEmail(locale, to, validated), nil
	default:
		return Email{}, fmt.Errorf(
			"%w: %s version %d",
			ErrUnknownTemplate,
			canonical.ID,
			canonical.Version,
		)
	}
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
	return Email{To: to, Subject: payload["subject"], Body: body, HTMLBody: htmlBody, ListUnsubscribe: "<" + payload["unsubscribeUrl"] + ">", OneClickUnsubscribe: true}
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

func brandedCodeEmailHTML(locale, church, heading, message, code, expiry, footer string) string {
	return fmt.Sprintf(`<!doctype html><html lang="%s"><body style="margin:0;background:#fbf5eb;color:#342d2b;font-family:Arial,'Noto Sans TC',sans-serif"><table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="background:#fbf5eb;padding:32px 16px"><tr><td align="center"><table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="max-width:560px;background:#fffdf9;border:1px solid #eaded2;border-radius:8px"><tr><td style="padding:32px"><p style="margin:0 0 28px;color:#b94f47;font-size:16px;font-weight:700">%s</p><h1 style="margin:0 0 16px;font-size:26px;line-height:1.35">%s</h1><p style="margin:0 0 24px;color:#665c58;font-size:16px;line-height:1.75">%s</p><p style="margin:0 0 20px;padding:18px;background:#f7ebe5;border-radius:6px;text-align:center;font-size:32px;font-weight:700;letter-spacing:8px">%s</p><p style="margin:0;color:#665c58;font-size:14px;line-height:1.65">%s</p><p style="margin:32px 0 0;padding-top:20px;border-top:1px solid #eaded2;color:#827773;font-size:13px;line-height:1.65">%s</p></td></tr></table></td></tr></table></body></html>`,
		html.EscapeString(locale), html.EscapeString(church), html.EscapeString(heading), html.EscapeString(message),
		html.EscapeString(code), html.EscapeString(expiry), html.EscapeString(footer))
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
