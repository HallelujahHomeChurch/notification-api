# Local Email Link Logging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow local notification-api runs to explicitly log email bodies so account verification and password-reset links can be used in end-to-end tests, while keeping the default output token-free.

**Architecture:** Add one opt-in `LOG_EMAIL_BODY` configuration value. `cmd/server` passes it to the existing `notify.LogSender`; the sender emits the already-rendered body only when enabled. SMTP delivery, queue implementations, HTTP routes, and production defaults remain unchanged.

**Tech Stack:** Go standard library, existing notification-api test suite, environment configuration.

## Global Constraints

- Do not add dependencies, containers, queues, endpoints, or persistent storage.
- `LOG_EMAIL_BODY` must default to `false`.
- The default log line must not contain `Email.Body`, including verification or password-reset tokens.
- Local documentation must show `LOG_EMAIL_BODY=true` only for development and must warn against production use.
- Use TDD: write and run a failing test before changing production code.

---

### Task 1: Add opt-in body logging to the existing local sender

**Files:**
- Create: `internal/notify/sender_test.go`
- Modify: `internal/notify/config.go:8-44`
- Modify: `internal/notify/sender.go:13-26`
- Modify: `cmd/server/main.go:83-86`
- Modify: `.env.example:16-17`
- Modify: `README.md:7-17`

**Interfaces:**
- Consumes: `notify.Config.LogEmailBody bool` loaded from `LOG_EMAIL_BODY`.
- Produces: `notify.LogSender{Logger: logger, LogBody: cfg.LogEmailBody}`.
- Behavioral contract: `LogSender.Send(context.Context, Email) error` always logs recipient and subject; it logs `Email.Body` only when `LogBody` is true.

- [ ] **Step 1: Write the failing sender test**

Create `internal/notify/sender_test.go` with these two tests. The second test must fail before the production change because `LogSender` does not yet expose `LogBody` or write the body.

```go
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
```

- [ ] **Step 2: Run the focused test to verify it fails**

Run:

```bash
go test ./internal/notify -run TestLogSenderLogsBodyWhenEnabled -count=1
```

Expected: compile failure because `LogSender` has no `LogBody` field. Do not continue if it passes.

- [ ] **Step 3: Add the minimal configuration and sender behavior**

Add `LogEmailBody bool` to `Config` and load it with the existing boolean helper:

```go
LogEmailBody: boolEnv("LOG_EMAIL_BODY", false),
```

Extend `LogSender` and its existing `Send` method without changing SMTP behavior:

```go
type LogSender struct {
	Logger  *log.Logger
	LogBody bool
}

func (s LogSender) Send(_ context.Context, email Email) error {
	logger := s.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("notification email accepted to=%s subject=%q", email.To, email.Subject)
	if s.LogBody {
		logger.Printf("notification email body to=%s:\\n%s", email.To, email.Body)
	}
	return nil
}
```

Pass the value from `newSender`:

```go
return notify.LogSender{Logger: logger, LogBody: cfg.LogEmailBody}
```

Add the following adjacent to the SMTP settings in `.env.example`:

```dotenv
# Local development only. Prints verification/reset links (and their tokens) to stdout.
LOG_EMAIL_BODY=false
```

Update the local development section of `README.md` with:

```markdown
Set `LOG_EMAIL_BODY=true` only for local end-to-end tests to print verification and password-reset links. It is `false` by default and must remain disabled outside local development because links contain single-use tokens.
```

- [ ] **Step 4: Run focused and full validation**

Run:

```bash
gofmt -w internal/notify/config.go internal/notify/sender.go internal/notify/sender_test.go cmd/server/main.go
go test ./internal/notify -count=1
go test ./...
go build -o /tmp/notification-api-server ./cmd/server
```

Expected: all tests pass and the server binary builds.

- [ ] **Step 5: Run the local delivery smoke test**

Restart the local notification-api process with the explicit dev-only setting:

```bash
PORT=8081 QUEUE_DRIVER=memory REDIS_ADDR=127.0.0.1:6379 LOG_EMAIL_BODY=true go run ./cmd/server
```

Trigger an account-api registration or forgot-password request. Expected notification-api output includes `notification email body` and a `http://127.0.0.1:5173/verify-email?token=...` or `/reset-password?...` link. Restart without `LOG_EMAIL_BODY` and confirm that line is absent.

- [ ] **Step 6: Commit the notification-api change**

```bash
git add .env.example README.md cmd/server/main.go internal/notify/config.go internal/notify/sender.go internal/notify/sender_test.go docs/superpowers/plans/2026-07-10-local-email-link-logging.md
git commit -m "feat: log local email links on opt-in"
```

## Self-Review

- Scope coverage: the plan covers the approved local-only email-body output, default token safety, test coverage, documentation, and a real process smoke test.
- Placeholder scan: no unresolved requirements, generic test steps, or implicit interfaces remain.
- Type consistency: `Config.LogEmailBody` maps directly to `LogSender.LogBody`; both are `bool` and are used only for the non-SMTP sender.
