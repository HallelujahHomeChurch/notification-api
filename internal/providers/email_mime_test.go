package providers

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestMIMEHeadersUnchanged(t *testing.T) {
	var message bytes.Buffer
	payload := DeliveryPayload{
		Recipient:           "person@example.test",
		Subject:             "測試",
		Body:                "first\n測試\n",
		MessageID:           "<delivery-1@notification.alive.org.tw>",
		ListUnsubscribe:     "<https://www.alive.org.tw/newsletter/unsubscribe>",
		OneClickUnsubscribe: true,
	}
	if err := writeEmailMIME(&message, "noreply@alive.org.tw", "", payload); err != nil {
		t.Fatal(err)
	}
	want := "From: noreply@alive.org.tw\r\n" +
		"To: person@example.test\r\n" +
		"Subject: =?UTF-8?q?=E6=B8=AC=E8=A9=A6?=\r\n" +
		"Message-ID: <delivery-1@notification.alive.org.tw>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"List-Unsubscribe: <https://www.alive.org.tw/newsletter/unsubscribe>\r\n" +
		"List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"first\r\n=E6=B8=AC=E8=A9=A6\r\n"
	if message.String() != want {
		t.Fatalf("MIME = %q, want %q", message.String(), want)
	}
}

func TestSMTPRejectsInjectedMIMEHeadersBeforeDial(t *testing.T) {
	provider := NewSMTP(SMTPConfig{Addr: "127.0.0.1:1", From: "noreply@alive.org.tw"})
	for _, tc := range []struct {
		name   string
		inject func(*DeliveryPayload)
	}{
		{"subject", func(p *DeliveryPayload) { p.Subject = "Subject\r\nBcc: other@example.test" }},
		{"unsubscribe", func(p *DeliveryPayload) { p.ListUnsubscribe = "https://example.test\r\nBcc: other@example.test" }},
		{"message ID", func(p *DeliveryPayload) { p.MessageID = "<delivery@example.test>\r\nBcc: other@example.test" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := validEmail()
			tc.inject(&payload)
			_, err := provider.Send(context.Background(), payload)
			var failure *ProviderError
			if !errors.As(err, &failure) || failure.Kind != ErrorInvalidEndpoint || failure.Operation != "validate" {
				t.Fatalf("Send() error = %v, want validation failure", err)
			}
		})
	}
}
