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
Worker mode also requires SMTP and VAPID configuration; there is no log-only
delivery fallback.

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
- `worker`: database, encryption key, Service Bus, SMTP, and VAPID settings

Production always rejects the development caller header and Service Bus
connection strings; managed identity uses `SERVICEBUS_NAMESPACE`.

Email delivery is at-least-once. Retries reuse a stable Internet `Message-ID`,
while the PostgreSQL idempotency key prevents duplicate notification intents.
SMTP does not provide an exactly-once delivery guarantee.

Web Push uses `engagement.web-push`, a `web_push` target containing serialized
`PushSubscription` JSON, and a payload with `title`, `body`, and optional
`actionUrl`. HTTP 404/410 permanently invalidate the endpoint; HTTP 429 and
5xx responses use the existing durable retry policy.

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

Web Push example:

```json
{
  "templateId": "engagement.web-push",
  "channel": "web_push",
  "target": {
    "type": "web_push",
    "address": "{\"endpoint\":\"https://push.example/...\",\"keys\":{\"p256dh\":\"...\",\"auth\":\"...\"}}"
  },
  "locale": "zh-Hant",
  "payload": {
    "title": "最新消息",
    "body": "教會近況與重要公告",
    "actionUrl": "https://www.alive.org.tw/zh-Hant/news"
  },
  "resource": {"type": "campaign", "id": "campaign-id"}
}
```

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

## Application logging

`LOG_LEVEL=debug|info|warn|error` defaults to `info`; invalid values stop
startup. This controls application slog and default standard-log output.
Persisted audit records remain independent of this setting.

Provider success/failure events use their existing dedicated logger at every
application level: the live provider failure ratio still requires both counts.

## SMTP quotas and acceptance safety

All production SMTP workers reserve a shared PostgreSQL quota slot before sending. Defaults are 20 attempts/minute and the verified ACS quota of 100 attempts/hour, with 5/minute and 20/hour reserved for non-campaign messages. `SMTP_EMAILS_PER_MINUTE`, `SMTP_EMAILS_PER_HOUR`, `SMTP_TRANSACTIONAL_RESERVE_PER_MINUTE`, and `SMTP_TRANSACTIONAL_RESERVE_PER_HOUR` override these validated values; minute defaults are conservative and are not a claim about the actual ACS minute quota. Windows count provider starts, including rejected and unknown attempts, conservatively. Capacity unavailable and provider-wide cooldowns defer through the existing outbox, without consuming the five-general-failure budget or blocking Web Push. Original message expiry remains authoritative.

Each Email claim records sanitized attempt outcomes; no target, content or raw SMTP response is copied. Campaign sends stop on unknown/incomplete acceptance for reconciliation; account messages retain the existing at-least-once retry policy. Accepted-attempt evidence recovers a failed receipt write without sending again. Attempt rows cascade with the parent delivery under existing 730-day message retention.

The existing caller-scoped status GET adds `safeToRetry`. Only complete, retained, unexpired, definitively rejected Email history can return true; legacy, accepted, ambiguous or incomplete history returns false. Missing fields must be treated as false by consumers. No historical failures are replayed by this migration or worker change.

## In-app inbox

Inbox is independent of Email/Web Push, and disabled by default. Enable only after
both business owners implement the bounded visibility contract and the WWW
Gateway permits the exact human routes in `docs/openapi.yaml`:

- `NOTIFICATION_INBOX_ENABLED=true`
- `NOTIFICATION_INBOX_OPERATIONS_VISIBILITY_URL`: Dapr invocation URL ending in `/priv/operations/inbox-visibility`
- `NOTIFICATION_INBOX_WEBSITE_VISIBILITY_URL`: Dapr invocation URL ending in `/priv/website/inbox-visibility`

The existing hash-key ring signs purpose-separated actor-bound cursors and
15-minute read snapshots. No additional secret is required. If the local Dapr
sidecar uses API-token authentication, its existing `DAPR_API_TOKEN` is attached
to visibility calls. URLs must point to the authenticated local sidecar, which
injects the Notification caller identity at the owner; direct unauthenticated
business-service URLs are not supported deployment configurations.

The inbox producer accepts only exact `operations-api` and `engagement-api`
Dapr identities and returns indexed `accepted`, `duplicate`, `expired`, or
`cleaned` outcomes after transaction commit. Only the first two represent a
durable reminder; the last two are terminal skips. Original events expire after
90 days. Owner titles and typed actions are validated on every read and are never
stored. Pages scan at most 500 candidates; follow `nextCursor` even for a short or
empty page. Exact summary counts have a separate deadline and fail unavailable
if incomplete; full-page reads do not require a successful count.

Private owner visibility payloads are `{actorId,locale,items:[{id,resource}]}` (max 100),
and responses `{items:[{id,visible,projection?}]}`; existing `data` envelopes are
unwrapped. Owners must return exactly one decision for every input. Projection
and action schemas are recorded in OpenAPI. All inbox responses use the existing
`{data,meta:{requestId},error}` envelope and `Cache-Control: no-store`.

List and summary accept optional `locale` from the five product locales, default
`zh-Hant`. Locale affects presentation only and never grants edition access; it
is carried to the business owner and bound into pagination cursors.
