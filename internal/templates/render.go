package templates

import "fmt"

type Email struct {
	To      string
	Subject string
	Body    string
}

func RenderEmail(definition Definition, locale, to string, payload map[string]string) (Email, error) {
	canonical, err := Resolve(definition.ID, definition.Channel)
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

	switch canonical.ID {
	case "account.verify-email":
		return renderVerificationEmail(locale, to, validated["verifyUrl"]), nil
	case "account.reset-password":
		return renderPasswordResetEmail(locale, to, validated["resetUrl"]), nil
	default:
		return Email{}, fmt.Errorf("%w: %s", ErrUnknownTemplate, canonical.ID)
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
