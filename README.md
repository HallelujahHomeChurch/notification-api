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

## Account data-request notices

Account alone may send `account.dsr-received`,
`account.dsr-information-required`, `account.dsr-completed`, and
`account.dsr-action-required` (v1, email, delivery TTL 24 hours). They are
transactional and use the existing durable/idempotent send path, without
marketing eligibility or unsubscribe controls. No administrator subscriptions
are enabled by these templates.

All four require exactly this payload:

```json
{
  "requestUrl": "https://account.alive.org.tw/data-requests",
  "requestId": "0199bb0e-10a0-7e07-b25e-221d331b6d13",
  "requestType": "access_export"
}
```

`requestType` is `access_export`, `correction`, `restrict_processing`, or
`erasure`; `requestId` must be a nonzero UUID. The URL must point exactly to
`/data-requests` on the production/test Account host (or a local HTTP origin),
without query or fragment. Extra fields, including internal notes and request
contents, are rejected. `resource: {type: "account", id: <Account user ID>}`
and `subjectAccountId` follow the existing Account MailService contract.
Use a stable idempotency key per request lifecycle event.

Locales are `zh-Hant`, `zh-Hans`, and `en`; others fall back to English.
Erasure completion contains a reference and `support@alive.org.tw` contact,
without a portal link or sign-in instruction for the deleted account.

DSR decision notices `account.dsr-rejected` and `account.dsr-extended` use the same caller-scoped v1 lifecycle contract: only `account-api`, `requestUrl`, `requestId` and `requestType`. They direct the subject to the protected decision/reason/deadline in Account; no decision text, evidence or ZIP travels in email. Adding templates alone does not enable extensions or statutory deadline policy. Existing `GET /priv/notifications/{messageId}` remains the caller-scoped delivery-state contract (`sent` describes provider acceptance, not recipient receipt/read).

## Temporary A4 owner inventory

The immutable image also packages `/app/dsr-inventory`. After merged CI/CD,
its owner operator may execute it with explicit UUID message IDs freshly read
from Account `dsr_notifications.message_id`; it uses only resident `DATABASE_URL`.
It accepts only the exact `hhc-pg.postgres.database.azure.com:5432/notification`
TLS target and verifies the hostname against system roots. It does not start
API, worker, migration, queue, or provider code.

The read-only repeatable-read snapshot requires every requested ID to match
`account-api` and the six exact Account DSR templates. It exports bounded
metadata for messages, linked deliveries/outbox, observed delivery fences, and
schema columns/FKs/triggers; other DSR messages are `unknownNotApproved`.
The metadata mode never prints recipients, payloads, provider IDs, or errors. Unknown FK shape,
missing IDs, query failure, or bounds prevent completed output. Fence status is
an observation, not a lock or approval to delete.

Output is buffered JSON between `DSR_INVENTORY_V1_BEGIN`, `SHA256 <hex>` (the
SHA-256 of the exact JSON line), and `DSR_INVENTORY_V1_COMPLETE`. Capture stdout
in a restricted local file and verify the whole frame/hash before using it.
The caps are 2 minutes, 10,000 total rows, and 4 MiB JSON. Remove this temporary
command, tests, and Docker binary after A5 acceptance.

### Temporary A5 protected backup and exact retirement

Two explicit operator modes extend that command:

```text
/app/dsr-inventory --backup <base64-manifest>
/app/dsr-inventory --retire <base64-manifest>
```

The bounded manifest is standard base64 of one JSON object with exactly these
collections and fields: `messages: [{id, xmin}]`,
`deliveries: [{id, messageId, xmin}]`, `outbox: [{id, deliveryId, xmin}]`.
Every collection must contain the three exact A4-reviewed IDs and links compiled
into this temporary command. Only canonical positive decimal PostgreSQL xmin
values may vary. Unknown/duplicate JSON fields, duplicate IDs, expanded scope,
changed links, invalid versions, extra arguments or more than 4 KiB decoded
manifest fail before connecting. The command sorts collections by ID; its
`manifestSHA256` hashes compact Go JSON encoding of that normalized manifest.

`--backup` remains a read-only repeatable-read transaction, takes no fence locks,
and refuses held delivery fences or drift. It returns **protected raw database
rows**, including stored ciphertext and provider fields, without decryption.
The separate `DSR_BACKUP_V1_BEGIN` / `DSR_BACKUP_V1_COMPLETE` frame and
`protectedBackup: true` distinguish it from metadata output. Root must capture
stdout directly into a 0600 file on the existing FileVault-protected filesystem;
never display or publish the backup. Each `contentSHA256` hashes Go
`json.Marshal([]json.RawMessage)` of the full rows ordered by ID. Restore only
into an isolated matching schema, reselect `row_to_json` in UTC ordered by ID,
and compare these hashes. xmin is system metadata in the manifest and is not
part of restorable row content. Verify the complete protected backup and this
isolated restore before any live retirement.

`--retire` takes nonblocking transaction advisory delivery fences in sorted ID
order, then the ordinary DELETE table lock mode and parent-first sorted row
locks. Ordinary DML remains compatible with the table lock; new children of
locked rows and concurrent DDL are blocked. Fresh guarded reads require the
exact sent/sent/published rows, null leases, fixed ID/version/link sets, no other
DSR messages, known foreign keys, the reviewed column hash, and no user triggers,
rewrite rules, RLS or partitioning. It deletes only exact ID+xmin matches in
outbox → deliveries → messages order, checks each RETURNING set, and commits
one transaction. Any database guard/delete failure rolls back all three tables.
No ordinary cleanup, queue operation, provider call, or decryption is invoked.

Only a postcommit metadata receipt uses the separate
`DSR_RETIREMENT_V1_BEGIN` / `DSR_RETIREMENT_V1_COMPLETE` frame. A missing receipt
or connection/output failure can leave commit outcome uncertain: reconcile the
exact rows through a fresh metadata observation before deciding the next step.
Never infer rollback from a lost receipt. TLS, target, execution/output bounds
and the original UUID-only read-only inventory remain in force. Remove all
these temporary modes with the A4 command after A5 acceptance.
