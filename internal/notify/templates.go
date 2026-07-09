package notify

import "fmt"

func BuildEmail(message Message) (Email, error) {
	switch message.Template {
	case TemplateEmailVerification:
		return Email{
			To:      message.To,
			Subject: "Verify your HHC account",
			Body:    fmt.Sprintf("Use this link to verify your HHC account:\n\n%s\n", message.Data["verify_url"]),
		}, nil
	case TemplatePasswordReset:
		return Email{
			To:      message.To,
			Subject: "Reset your HHC account password",
			Body:    fmt.Sprintf("Use this link to reset your HHC account password:\n\n%s\n", message.Data["reset_url"]),
		}, nil
	default:
		return Email{}, fmt.Errorf("unsupported template")
	}
}
