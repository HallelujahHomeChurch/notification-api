# notification-api

Internal email notification service for HHC account workflows.

## Local Development

Default local mode avoids Azure:

```bash
cp .env.example .env
set -a && source .env && set +a
go run ./cmd/server
```

`cmd/server` remains the legacy worker runtime until the durable `api`, `worker`,
and `migrate` commands are assembled. It exposes process health only.

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
POST /priv/notifications/send
Dapr-Caller-App-Id: account-api
Idempotency-Key: account.verify-email:<operation-id>
Content-Type: application/json
```

```json
{
  "templateId": "account.verify-email",
  "channel": "email",
  "target": {
    "type": "email",
    "address": "user@example.com"
  },
  "locale": "zh-Hant",
  "payload": {
    "verifyUrl": "https://account.alive.org.tw/verify-email?token=..."
  },
  "resource": {
    "type": "account",
    "id": "user-id"
  }
}
```

See `docs/openapi.yaml` for the complete private contract.

## Checks

```bash
go test ./...
go build ./cmd/server
```
