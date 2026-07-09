# notification-api

Internal email notification service for HHC account workflows.

## Local Development

Default local mode avoids Azure:

```bash
cp .env.example .env
set -a && source .env && set +a
go run ./cmd/server
```

`QUEUE_DRIVER=memory` runs the enqueue endpoint and worker in one process. It still uses Redis for email cooldown and daily caps.

If `SMTP_ADDR` is empty, emails are logged instead of sent.

Set `LOG_EMAIL_BODY=true` only for local end-to-end tests to print verification and password-reset links. It is `false` by default and must remain disabled outside local development because links contain single-use tokens.

## Azure Queue Mode

Production uses Azure Service Bus:

```bash
QUEUE_DRIVER=servicebus
SERVICEBUS_CONNECTION_STRING='Endpoint=sb://...'
SERVICEBUS_QUEUE_NAME=notifications-email
```

For local Azure-compatible testing, use the official Azure Service Bus emulator and point `SERVICEBUS_CONNECTION_STRING` at the emulator. The app code is unchanged.

## API

Internal endpoint:

```http
POST /priv/notification/v1/email
X-Internal-Token: <optional shared token>
Content-Type: application/json
```

```json
{
  "template": "email_verification",
  "to": "user@example.com",
  "data": {
    "verify_url": "https://account.alive.org.tw/verify-email?token=..."
  }
}
```

Templates:

- `email_verification` requires `data.verify_url`
- `password_reset` requires `data.reset_url`

## Checks

```bash
go test ./...
go build ./cmd/server
```
