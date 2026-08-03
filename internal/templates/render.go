package templates

import "fmt"

type Email struct {
	To      string
	Subject string
	Body    string
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
		return renderVerificationEmail(locale, to, validated["verifyUrl"]), nil
	case canonical.ID == "account.reset-password" && canonical.Version == 1:
		return renderPasswordResetEmail(locale, to, validated["resetUrl"]), nil
	case canonical.ID == "account.oauth-link-confirmation" && canonical.Version == 1:
		return renderOAuthLinkConfirmationEmail(locale, to, validated["confirmUrl"], validated["provider"]), nil
	case canonical.ID == "account.oauth-onboarding-code" && canonical.Version == 1:
		return renderOAuthOnboardingCodeEmail(locale, to, validated["code"]), nil
	default:
		return Email{}, fmt.Errorf(
			"%w: %s version %d",
			ErrUnknownTemplate,
			canonical.ID,
			canonical.Version,
		)
	}
}

func renderOAuthOnboardingCodeEmail(locale, to, code string) Email {
	switch locale {
	case "zh-Hant":
		return Email{To: to, Subject: "驗證您的 HHC 帳戶 Email", Body: "您的 HHC 帳戶驗證碼是：" + code + "\n\n驗證碼將於 10 分鐘後失效。請勿將驗證碼提供給他人。\n"}
	case "zh-Hans":
		return Email{To: to, Subject: "验证您的 HHC 帐户 Email", Body: "您的 HHC 帐户验证码是：" + code + "\n\n验证码将在 10 分钟后失效。请勿将验证码提供给他人。\n"}
	default:
		return Email{To: to, Subject: "Verify your HHC account email", Body: "Your HHC account verification code is: " + code + "\n\nThe code expires in 10 minutes. Do not share it with anyone.\n"}
	}
}

func renderOAuthLinkConfirmationEmail(locale, to, confirmURL, provider string) Email {
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

func renderVerificationEmail(locale, to, verifyURL string) Email {
	switch locale {
	case "zh-Hant":
		return Email{To: to, Subject: "驗證您的 HHC 帳戶", Body: "請使用以下連結驗證您的 HHC 帳戶：\n\n" + verifyURL + "\n"}
	case "zh-Hans":
		return Email{To: to, Subject: "验证您的 HHC 帐户", Body: "请使用以下链接验证您的 HHC 帐户：\n\n" + verifyURL + "\n"}
	default:
		return Email{To: to, Subject: "Verify your HHC account", Body: "Use this link to verify your HHC account:\n\n" + verifyURL + "\n"}
	}
}

func renderPasswordResetEmail(locale, to, resetURL string) Email {
	switch locale {
	case "zh-Hant":
		return Email{To: to, Subject: "重設您的 HHC 帳戶密碼", Body: "請使用以下連結重設您的 HHC 帳戶密碼：\n\n" + resetURL + "\n"}
	case "zh-Hans":
		return Email{To: to, Subject: "重置您的 HHC 帐户密码", Body: "请使用以下链接重置您的 HHC 帐户密码：\n\n" + resetURL + "\n"}
	default:
		return Email{To: to, Subject: "Reset your HHC account password", Body: "Use this link to reset your HHC account password:\n\n" + resetURL + "\n"}
	}
}
