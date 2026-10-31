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

The migration Job now references `notification-migrate-database-url`, role `notification_migrate` (cap1).
API and persistent-worker credentials stay separate. This configuration is a
pending dependency: provision the capped login/secret and its narrow identity
read permission with Job admission paused before release. No credentials or
role assignments were created here; see azure-infra PR144 for the operator
preview and inventory verification.

## Release image handoff

The release Verify job builds and scans one `linux/amd64` image. The plan job
loads that image from the exact artifact ID emitted by Verify; it does not
rebuild the source in ACR. The handoff checks the full commit, platform, archive
checksums and image identities before loading. After push, the immutable digest
is pulled and checked against the verified image ID before the deployment plan.
The release scan gates artifact export and publication; the same verified bytes
are not scanned again after promotion. GitHub's same-run artifact selection supplies provenance;
checksums alone are corruption detection, not signatures.

PR CI exercises rejection cases and a real image export/load round trip. Release
transport adds gzip/save, upload, download and load work; measure these costs
against the removed ACR build before claiming a release-time improvement.
Migration, worker rollout, health gates and rollback remain in the release flow.

CI executes integration packages serially (`go test -p 1`): migration fixtures
share a database-wide advisory lock even when their schemas differ. This keeps
the existing lock deadline assertions meaningful; all cases still run once.

## Application health and dependency diagnostics

Application liveness/readiness describes the initialized HTTP process, not remote
dependency availability. Startup validation remains mandatory; shutdown uses the
existing HTTP drain and owned worker cancellation/join. A DB, Redis or Dapr outage
does not make a responsive initialized API unready. Actual business requests
retain their existing authorization, durability and fail-closed behavior.

An independent runtime worker checks each dependency with its own two-second
deadline, once per 30-second cycle without overlapping probes. In API mode it
emits an initial sample, outcome changes and a five-minute heartbeat (including
unchanged failures). It emits
`dependency_health` with `dependency`, `outcome` and `elapsed_ms`; no raw errors,
DSNs, tokens or response bodies are logged. Outcomes are `ok`, `deadline`,
`cancelled`, `unavailable` and `not_configured`. DB is monitored in every serving
mode; Account additionally monitors Redis. Dapr outbound health is sampled only
when the injected `DAPR_HTTP_PORT` is configured, using `DAPR_API_TOKEN` when set;
it does not prove downstream applications are available. Redirects are rejected.

Keep `LOG_LEVEL=info` to retain baseline/recovery heartbeats; dependency failures
are Warn. `dependency_monitor_stopped` records the worker exit. Alert on a missing
API per-replica heartbeat for more than six minutes while the replica is expected to
serve; silence is unknown, not healthy. These logs complement request/trace and
platform sidecar lifecycle evidence and never determine AP probe responses.
Alert deployment and production acceptance remain separate from this source change.

## HTTP diagnostic correlation

API responses and request context share a single `X-HHC-Request-ID`: one supplied
ASCII `[A-Za-z0-9._:-]{1,128}` value, or a generated 128-bit random ID for missing,
duplicate or invalid input. Only one valid lowercase version-00 `traceparent`
with nonzero trace/span IDs and flags `00`/`01` enters diagnostic context.
Supplied headers remain
unchanged for independent audit-provenance validation; diagnostic IDs never
establish caller identity or replace required supplied audit evidence.

Standalone worker dependency logging retains its existing 30-second events;
its missing-heartbeat threshold remains 90 seconds. API monitoring silence beyond
six minutes (five-minute heartbeat plus sampling/ingestion allowance) is unknown,
not healthy. LOG_LEVEL filtering remains unchanged; retain info for heartbeats.

## Immutable image scan evidence

Release Verify scans the built image once into Trivy JSON before artifact export. The same validated report gates fixable High/Critical vulnerabilities and
summarizes unfixed findings. High/Critical secrets also block without printing
matched content. Malformed reports, wrong image/version and scanner failures block.
The pinned scanner, vulnerability DB metadata, source dependency scan, CI image
scan remain. Exact same-run artifact identity and post-push image identity bind
the scan gate to the published bytes. No checks across different commits or
artifacts are removed.
