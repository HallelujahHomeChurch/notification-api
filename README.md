# notification-api

Internal notification delivery service for HHC account and engagement workflows.

## Local Development

Run PostgreSQL migrations, then start the API and worker as separate processes:

```bash
cp .env.example .env
set -a && source .env && set +a
go run ./cmd/notification migrate
go run ./cmd/notification api
go run ./cmd/notification worker
```

The durable runtime requires PostgreSQL and Service Bus. For local development,
use the Azure Service Bus emulator through `SERVICEBUS_CONNECTION_STRING`.
Worker mode also requires `SMTP_ADDR` and `SMTP_FROM`; there is no log-email
fallback.

## Azure Queue Mode

Production uses managed identity:

```bash
QUEUE_DRIVER=servicebus
SERVICEBUS_NAMESPACE='alive-notifications.servicebus.windows.net'
SERVICEBUS_QUEUE_NAME=notifications-email
```

Development can instead set `SERVICEBUS_CONNECTION_STRING`. The API requires
send permission and the worker requires receive permission.

## Runtime Modes

```text
notification-api api
notification-api worker
notification-api migrate
```

`api` serves the private HTTP API and runs outbox/retention loops. `worker`
consumes PeekLock deliveries and serves private health endpoints. `migrate`
runs embedded PostgreSQL migrations once and exits.

Configuration is validated per command:

- `migrate`: `DATABASE_URL`
- `api`: database, encryption/hash keys, allowed callers, and Service Bus
- `worker`: database, encryption key, Service Bus, and valid SMTP settings

Production always rejects the development caller header and Service Bus
connection strings; managed identity uses `SERVICEBUS_NAMESPACE`.

Email delivery is at-least-once. Retries reuse a stable Internet `Message-ID`,
while the PostgreSQL idempotency key prevents duplicate notification intents.
SMTP does not provide an exactly-once delivery guarantee.

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

## Production Operations

See the [production runbook](docs/runbook.md) for incident triage, queue and
kill-switch controls, credential rotation, DLQ replay, rollback, PostgreSQL
PITR/DR, alerts, and log-redaction requirements.

The [production acceptance checklist](docs/runbook.md#production-acceptance)
is the release gate for the notification dependency. Do not begin the
production `account-api` cutover until that checklist is complete. Static and
local verification do not constitute live acceptance.

## Checks

```bash
go test ./...
go vet ./...
go build ./cmd/notification
docker build -t notification-api:local .
```
