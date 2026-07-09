package notify

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func TestLogSenderDoesNotLogBodyByDefault(t *testing.T) {
	var output bytes.Buffer
	sender := LogSender{Logger: log.New(&output, "", 0)}
	email := Email{To: "user@example.com", Subject: "Verify", Body: "https://account.test/verify-email?token=secret-token"}

	if err := sender.Send(context.Background(), email); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if strings.Contains(output.String(), email.Body) {
		t.Fatalf("log unexpectedly contains email body: %q", output.String())
	}
}

func TestLogSenderLogsBodyWhenEnabled(t *testing.T) {
	var output bytes.Buffer
	sender := LogSender{Logger: log.New(&output, "", 0), LogBody: true}
	email := Email{To: "user@example.com", Subject: "Verify", Body: "https://account.test/verify-email?token=secret-token"}

	if err := sender.Send(context.Background(), email); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if !strings.Contains(output.String(), email.Body) {
		t.Fatalf("log = %q, want email body", output.String())
	}
}
