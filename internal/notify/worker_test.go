package notify

import "testing"

func TestBuildEmailAdaptsLegacyVerificationTemplate(t *testing.T) {
	email, err := BuildEmail(Message{
		Template: TemplateEmailVerification,
		To:       "user@example.test",
		Data: map[string]string{
			"verify_url": "https://account.alive.org.tw/verify-email?token=opaque",
		},
	})
	if err != nil {
		t.Fatalf("BuildEmail() error = %v", err)
	}
	if email.Subject != "Verify your HHC account" {
		t.Fatalf("BuildEmail() subject = %q", email.Subject)
	}
	if email.Body != "Use this link to verify your HHC account:\n\nhttps://account.alive.org.tw/verify-email?token=opaque\n" {
		t.Fatalf("BuildEmail() body = %q", email.Body)
	}
}
