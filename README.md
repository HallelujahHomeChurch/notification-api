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

### SES transport foundation (cutover disabled)

SMTP remains the default. The worker can select `EMAIL_PROVIDER=ses` with
`SES_REGION`, `SES_ROLE_ARN`, `AZURE_CLIENT_ID`, `SES_ENTRA_TOKEN_SCOPE`,
`SES_FROM`, and `SES_CONFIGURATION_SET`. These are non-secret identifiers.
SES mode does not require SMTP configuration. It uses only the named Azure
managed identity, exchanges its scoped Entra token with regional STS, and caches
short-lived AWS credentials. AWS profiles, environment access keys and human
SSO sessions are not credential fallbacks. SES SMTP credentials are not used.

The SES client makes one API attempt per durable worker attempt. Credential
acquisition failures happen before submission; a transport failure after invoking
SES is conservatively acceptance-unknown. There is no automatic SMTP failover.
The existing ambiguous-acceptance policy still applies. SES acceptance means
queued by the provider, not delivered to an inbox. Raw MIME retains application
headers, but SES may replace Message-ID; the actual SES receipt and a non-personal
`hhc_delivery_id` message tag support later feedback correlation.

Migration 007 adds provider receipts to the email-attempt ledger. Existing SMTP
rows retain their legacy recovery representation. A process crash after recording
SES acceptance restores the actual receipt without sending again. Never replay
uncertain sends or run old SMTP workers against SES traffic during rollback;
fence and drain workers, reconcile uncertain attempts, then explicitly select the
previous provider through the normal release workflow.

This foundation does not provision trust, sender/DKIM, configuration sets or
feedback ingestion, raise quotas, change queue priority, activate SES, or enable
login verification. Those controls and live delivery acceptance remain required
before production cutover. Existing conservative pacing settings are unchanged;
selecting SES alone does not meet campaign/OTP throughput targets.

### SES feedback and technical suppression (activation disabled)

With `SES_FEEDBACK_QUEUE_URL`, `SES_FEEDBACK_DLQ_URL`, `SES_FEEDBACK_TOPIC_ARN`
and the federation settings present, the existing worker also consumes a private
SNS-to-SQS feedback queue. This can run while `EMAIL_PROVIDER=smtp`; blank
feedback settings preserve current startup. Both API and worker must use the
same provider selection because each intent pins its provider when created.

Migration 008 adds minimal event metadata, address HMAC blocks and retained-key
target aliases. The consumer validates the queue/account/topic/configuration-set
and matches the real SES receipt or a reserved uncertain SES attempt before
committing feedback. Only committed events are deleted; errors reach the DLQ.
Duplicate events are idempotent, conflicting duplicate IDs are rejected, and a
late delivery event does not remove a complaint or permanent-bounce block.
Unknown acceptance is reconciled without replaying the email.

Technical blocks apply to every Email template and both providers, independently
of newsletter consent. Temporary bounces do not create permanent blocks. HMAC
aliases preserve blocks across key rotation; key retirement fails while references
remain. Explicit verified Account erasure clears local matching HMACs; late
feedback cannot recreate erased identity data. SES-side suppression is separate.
No public callback, full provider payload or recipient address is stored in the
feedback ledger. See [feedback operations](docs/runbook.md#ses-feedback-operations).

The resource PR and runtime configuration must be reviewed and released before
these controls can be described as operational. This change does not enable SES,
change pacing or implement the deferred login-security features.

### Isolated delivery lanes (opt-in, not activated)

`NOTIFICATION_ISOLATED_QUEUES=false` preserves the current single queue. With
the setting enabled on both API and worker, each lane has its own outbox
dispatcher and sequential consumer within the existing processes:

| Lane | Queue name | Persisted classification |
| --- | --- | --- |
| Transactional email | `${SERVICEBUS_QUEUE_NAME}-transactional` | Email with a non-Engagement template |
| Campaign email | `${SERVICEBUS_QUEUE_NAME}-bulk` | Email with an `engagement.*` template |
| Web Push | `${SERVICEBUS_QUEUE_NAME}` | `web_push` channel |

The existing caller/template allowlist determines classification; callers cannot
request priority. A blocked campaign publisher or provider does not occupy the
transactional or Web Push consumer. Workers require the existing five-connection
DB pool minimum when using three lanes. This is a minimum for these consumers,
not a production connection-budget or scale acceptance result.

SMTP retains its existing minute/hour settings. SES uses separate rolling-second
and rolling-24-hour limits: `SES_EMAILS_PER_SECOND=1`, `SES_EMAILS_PER_DAY=200`,
`SES_TRANSACTIONAL_RESERVE_PER_SECOND=0`, and
`SES_TRANSACTIONAL_RESERVE_PER_DAY=20` by default. Shared PostgreSQL reservations
count rejected and unknown attempts conservatively and prevent campaigns from
using the transactional reserve. SES attempt counts exclude SMTP attempts;
provider cooldown remains conservative during rollback. Exhausted windows defer
durably; a consumer can wait at most one second for short SES pacing. Expiry is
rechecked after rendering/eligibility and bounds the provider deadline.

The fake-provider acceptance profile uses 5/second and 6,000/day, reserving
1/second and 1,000/day for transactions. This permits at most 5,000 campaign
attempts/day. These are test/planning settings, not approved SES quotas or live
performance. Before cutover, choose limits within the actual account/region
headroom, including other senders; this ledger cannot observe their usage.

Local acceptance uses a disposable PostgreSQL database, fake brokers and fake
providers. It exercises a 1,000-recipient batch with every 100 submissions within
60 seconds, an account-code submission within five seconds during the batch,
blocked-publisher isolation, rolling reservations, expiry, 429 retries, duplicate
delivery and conservative SMTP rollback. It does not send email or validate the
real Service Bus/provider/inbox path. Run it with the repository integration
suite and a loopback `TEST_DATABASE_URL`.

Bicep's `isolatedDeliveryQueues` parameter defaults to false. When explicitly
enabled later, it declares the two additional queues, passes the common runtime
setting and scales on all three queues. The release workflow passes the reviewed
`NOTIFICATION_ISOLATED_QUEUES` repository variable consistently to planning and
deployment; this change does not set that variable. See the
[activation and rollback gates](docs/runbook.md#isolated-delivery-activation-and-rollback).

The migration Job now references `notification-migrate-database-url`, role `notification_migrate` (cap1).
API and persistent-worker credentials stay separate. This configuration is a
pending dependency: provision the capped login/secret and its narrow identity
read permission with Job admission paused before release. No credentials or
role assignments were created here; see azure-infra PR144 for the operator
preview and inventory verification.
