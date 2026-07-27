# Notification API Production Microservice Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the current email MVP into a private, durable, independently deployable notification microservice, create its PostgreSQL database, and deploy its API and worker to Azure Container Apps.

**Architecture:** `notification-api` accepts notification intents through Dapr, writes the canonical message and delivery state to PostgreSQL, and publishes only a `deliveryId` to Azure Service Bus through a transactional outbox. A separately scalable worker consumes deliveries with at-least-once semantics and records provider receipts. The schema separates messages from deliveries so Web Push, FCM, and APNs can be added later without changing caller contracts.

**Tech Stack:** Go 1.25, PostgreSQL through `pgx/v5`, Azure Service Bus Go SDK, `DefaultAzureCredential`, Azure Container Apps, Dapr service invocation, KEDA, Bicep, GitHub Actions OIDC.

## Global Constraints

- Keep `notification-api` private; no public ingress or browser API is added.
- PostgreSQL is the source of truth. Service Bus is transport and DLQ, not business state.
- Delivery semantics are at-least-once; idempotent state transitions prevent duplicate provider sends where possible.
- Production uses Service Bus and managed identity. Connection strings and memory queue are development-only.
- Production configuration fails closed when DB, caller identity, queue, encryption keys, or email provider configuration is missing.
- Callers submit template data, not provider names, subjects, arbitrary HTML, or provider credentials.
- Template IDs are namespaced and versioned in code: `account.verify-email` v1 and `account.reset-password` v1.
- API and worker use one image with `api`, `worker`, and `migrate` commands, but deploy as separate ACA resources.
- Preserve future `web_push` and `mobile_push` channel boundaries without adding their SDKs, registration APIs, credentials, or endpoint tables.
- Use the existing private PostgreSQL server at `172.16.68.4`, but create an isolated `notification` database and `notification` login.
- Read `PG_ADMIN_PASSWORD` from `/Users/rayselfs/Projects/hhc/.env.json`; never print or commit it.
- Store the generated notification DB password locally as `NOTIFICATION_DB_PASSWORD` in that ignored JSON file with mode `0600`, and store production secrets in `alive-vault`.
- Do not add Redis to the notification service in v1. Durable throttling and suppression use PostgreSQL.
- Payload ciphertext is purged seven days after terminal status; delivery metadata is retained for 730 days.
- Every task uses TDD and ends in a focused commit.

---

## Target File Structure

```text
cmd/notification/main.go                 # api|worker|migrate entrypoint
internal/config/config.go                # fail-closed environment parsing
internal/contracts/types.go              # stable request/response and statuses
internal/crypto/envelope.go              # AES-GCM payload encryption and HMAC hashes
internal/database/database.go            # pgx-backed sql.DB lifecycle
internal/migrations/migrations.go        # embedded checksum migrations
internal/migrations/sql/001_initial.sql  # message, delivery, outbox, rate-limit schema
internal/integration/postgres_test.go    # real PostgreSQL migration and lease tests
internal/store/store.go                   # durable repository and leases
internal/templates/registry.go           # immutable template definitions
internal/templates/render.go             # localized email rendering
internal/providers/provider.go           # provider receipt and typed failure contract
internal/providers/smtp.go               # context-bounded SMTP implementation
internal/queue/queue.go                   # publisher/consumer interfaces
internal/queue/servicebus.go              # managed identity and local connection-string adapter
internal/service/service.go               # send/idempotency/status orchestration
internal/httpapi/handler.go               # Dapr-only routes, health, ready
internal/outbox/dispatcher.go             # DB outbox to Service Bus
internal/worker/worker.go                 # delivery consumer and retry transitions
internal/retention/worker.go              # sensitive payload purge and metadata retention
infra/main.bicep                         # Service Bus, API ACA, worker ACA, job, roles, probes
infra/main.bicepparam.example             # non-secret deployment parameters
.github/workflows/release.yml            # verify, build, migrate, deploy, ready wait
docs/openapi.yaml                         # private API contract
docs/runbook.md                           # deploy, DLQ, pause, replay, DR
```

## Stable Interfaces

### Send intent

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
    "verifyUrl": "https://account.alive.org.tw/verify-email?token=opaque"
  },
  "resource": {
    "type": "account",
    "id": "user-id"
  }
}
```

```json
{
  "data": {
    "messageId": "opaque-uuid",
    "status": "queued",
    "templateVersion": 1,
    "replayed": false
  },
  "meta": {
    "requestId": "opaque-uuid"
  },
  "error": null
}
```

### Read status

```http
GET /priv/notifications/{messageId}
Dapr-Caller-App-Id: account-api
```

Only the original caller can read a message. Provider payloads, recipient addresses, and encrypted template data are never returned.

### Idempotency

- Scope: `(caller_app_id, idempotency_key)`.
- Same key and canonical request hash: return the original message with `replayed=true`.
- Same key and different canonical request hash: return `409 idempotency_conflict`.
- The key is required, 1-200 visible ASCII characters, and retained for 30 days.

### Internal channel boundary

```go
type Provider interface {
	Send(context.Context, contracts.DeliveryPayload) (contracts.ProviderReceipt, error)
}

type Publisher interface {
	Publish(context.Context, uuid.UUID) error
}

type Consumer interface {
	Consume(context.Context, func(context.Context, uuid.UUID) error) error
}
```

`channel` is stored as text and validated by the template registry. Do not use a PostgreSQL enum.

---

### Task 1: Freeze Contracts And Production Configuration

**Files:**
- Create: `internal/contracts/types.go`
- Create: `internal/config/config_test.go`
- Replace: `internal/notify/config.go`
- Create: `internal/config/config.go`
- Create: `docs/openapi.yaml`
- Modify: `.env.example`

**Interfaces:**
- Produces: `contracts.SendRequest`, `contracts.SendResponse`, `contracts.MessageStatus`, `contracts.DeliveryStatus`, `config.Load()`.
- Consumes: no earlier task.

- [ ] **Step 1: Write failing configuration tests**

Cover:

```go
func TestLoadProductionRejectsMissingDatabaseURL(t *testing.T)
func TestLoadProductionRejectsMemoryQueue(t *testing.T)
func TestLoadProductionRejectsMissingProvider(t *testing.T)
func TestLoadDevelopmentAllowsServiceBusConnectionString(t *testing.T)
func TestLoadParsesAllowedCallers(t *testing.T)
```

- [ ] **Step 2: Run the focused tests**

Run:

```bash
go test ./internal/config -run TestLoad -count=1
```

Expected: fail because `internal/config` does not exist.

- [ ] **Step 3: Add the stable types and fail-closed configuration**

Use these environment names:

```text
ENVIRONMENT=development|production
NOTIFICATION_MODE=api|worker|migrate
PORT=8081
DATABASE_URL
NOTIFICATION_ALLOWED_CALLERS=account-api,hhc-web-api
NOTIFICATION_ALLOW_DEV_CALLER_HEADER=false
NOTIFICATION_DATA_ENCRYPTION_KEY
NOTIFICATION_HASH_KEY
QUEUE_DRIVER=servicebus|memory
SERVICEBUS_NAMESPACE
SERVICEBUS_QUEUE_NAME=notifications-email
SERVICEBUS_CONNECTION_STRING
SMTP_ADDR
SMTP_USERNAME
SMTP_PASSWORD
SMTP_FROM
NOTIFICATIONS_DISABLED=false
SHUTDOWN_TIMEOUT_SECONDS=30
```

Production requires `DATABASE_URL`, 32-byte decoded encryption key, at least 32 bytes of hash key material, non-empty allowed callers, `QUEUE_DRIVER=servicebus`, Service Bus namespace, SMTP address/from, and disabled dev caller headers.

- [ ] **Step 4: Document the exact private contract in OpenAPI**

Include only:

```text
GET  /health
GET  /ready
POST /priv/notifications/send
GET  /priv/notifications/{messageId}
```

- [ ] **Step 5: Verify**

Run:

```bash
go test ./internal/config -count=1
go test ./...
go vet ./...
```

- [ ] **Step 6: Commit**

```bash
git add .env.example docs/openapi.yaml internal/config internal/contracts internal/notify/config.go
git commit -m "feat: define durable notification contracts"
```

### Task 2: Add Encrypted PostgreSQL Ledger And Migrations

**Files:**
- Create: `internal/crypto/envelope.go`
- Create: `internal/crypto/envelope_test.go`
- Create: `internal/database/database.go`
- Create: `internal/migrations/migrations.go`
- Create: `internal/migrations/migrations_test.go`
- Create: `internal/migrations/sql/001_initial.sql`
- Create: `internal/integration/postgres_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: `config.Config`.
- Produces: `crypto.Encrypt`, `crypto.Decrypt`, `crypto.Hash`, `database.Open`, `migrations.Run`.

- [ ] **Step 1: Write failing crypto and migration tests**

Required crypto behavior:

```go
plaintext := []byte(`{"verifyUrl":"https://example.test/token"}`)
ciphertext, err := Encrypt(key, plaintext)
require.NoError(t, err)
require.NotEqual(t, plaintext, ciphertext)
decoded, err := Decrypt(key, ciphertext)
require.Equal(t, plaintext, decoded)
```

Migration test assertions:

```text
schema_migrations exists with checksum enforcement
notification_messages has unique(caller_app_id, idempotency_key)
notification_deliveries references notification_messages
notification_outbox references notification_deliveries
notification_rate_limits contains hashed bucket keys only
```

`internal/integration/postgres_test.go` uses build tag `integration` and
`TEST_DATABASE_URL`. It applies migrations twice, verifies checksum rejection,
and exercises PostgreSQL advisory locks and `FOR UPDATE SKIP LOCKED` against a
real PostgreSQL instance. Unit tests remain DB-independent.

- [ ] **Step 2: Add dependencies and run failing tests**

```bash
go get github.com/jackc/pgx/v5/stdlib@latest
go get github.com/google/uuid@latest
go test ./internal/crypto ./internal/migrations -count=1
```

- [ ] **Step 3: Implement AES-256-GCM and HMAC-SHA256 helpers**

Ciphertext includes a fresh random nonce prefix. `Hash` returns lowercase hex HMAC-SHA256 and is used for recipient hashes and canonical request hashes.

- [ ] **Step 4: Create the schema**

`001_initial.sql` creates:

```text
notification_messages
  id uuid primary key
  caller_app_id text not null
  idempotency_key text not null
  request_hash text not null
  template_id text not null
  template_version integer not null
  channel text not null
  target_type text not null
  target_hash text not null
  target_ciphertext bytea not null
  payload_ciphertext bytea not null
  resource_type text not null
  resource_id text not null
  status text not null
  created_at, updated_at, terminal_at, payload_purged_at timestamptz

notification_deliveries
  id uuid primary key
  message_id uuid not null references notification_messages
  channel text not null
  endpoint_ref text null
  provider text not null
  status text not null
  attempt_count integer not null
  next_attempt_at, lease_expires_at, sent_at timestamptz
  provider_message_id, last_error_code text
  created_at, updated_at timestamptz

notification_outbox
  id uuid primary key
  delivery_id uuid not null references notification_deliveries
  status text not null
  attempt_count integer not null
  next_attempt_at, lease_expires_at, published_at timestamptz
  created_at, updated_at timestamptz

notification_rate_limits
  bucket_key text primary key
  count bigint not null
  expires_at timestamptz not null
```

Use text checks for supported lifecycle states, partial indexes for queued work, and advisory-lock checksum migrations copied from the proven Asset API pattern with lock name `hhc_notification_api_migrations`.

- [ ] **Step 5: Verify**

```bash
go test ./internal/crypto ./internal/migrations -count=1
go test ./...
go vet ./...
TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/notification_test?sslmode=disable \
  go test -tags=integration ./internal/integration -count=1
```

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/crypto internal/database internal/migrations internal/integration
git commit -m "feat: add encrypted notification ledger"
```

### Task 3: Add Versioned Template Registry

**Files:**
- Create: `internal/templates/registry.go`
- Create: `internal/templates/registry_test.go`
- Create: `internal/templates/render.go`
- Create: `internal/templates/render_test.go`
- Delete: `internal/notify/templates.go`

**Interfaces:**
- Consumes: `contracts.SendRequest`.
- Produces: `templates.Resolve(templateID, channel)`, `templates.RenderEmail`.

- [ ] **Step 1: Write failing registry tests**

Cover:

```text
account.verify-email resolves to version 1 and email
account.reset-password resolves to version 1 and email
account-api is allowed; hhc-web-api is rejected for account templates
unknown payload fields are rejected
missing required URL is rejected
zh-Hant, zh-Hans, and en render localized subject/body
unsupported locale falls back to en
```

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/templates -count=1
```

- [ ] **Step 3: Implement immutable definitions**

Each definition contains:

```go
type Definition struct {
	ID              string
	Version         int
	Channel         string
	AllowedCallers  map[string]bool
	RequiredFields  map[string]bool
	AllowedFields   map[string]bool
	SupportedLocale map[string]bool
}
```

The renderer receives only validated fields. It never accepts caller-provided subject or body.

- [ ] **Step 4: Verify and commit**

```bash
go test ./internal/templates -count=1
go test ./...
git add internal/templates internal/notify/templates.go
git commit -m "feat: add versioned notification templates"
```

### Task 4: Implement Durable Idempotent Send And Status Service

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/store_test.go`
- Create: `internal/service/service.go`
- Create: `internal/service/service_test.go`
- Delete: `internal/notify/service.go`
- Delete: `internal/notify/types.go`

**Interfaces:**
- Consumes: crypto helpers, template registry, database schema.
- Produces: `service.Send(ctx, caller, idempotencyKey, request)` and `service.Get(ctx, caller, messageID)`.

- [ ] **Step 1: Write failing service tests**

Cover:

```text
new request creates message, one delivery, and one outbox row in one transaction
same caller/key/hash returns original message with replayed=true
same caller/key/different hash returns ErrIdempotencyConflict
different callers may reuse the same idempotency key
status lookup rejects a caller other than the creator
recipient and payload are stored encrypted
rate-limited request creates no message
```

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/service ./internal/store -count=1
```

- [ ] **Step 3: Implement one transaction per send**

Normalize email, canonicalize the validated request, HMAC the canonical bytes, resolve the exact template version, check rate-limit buckets, and insert all three records atomically.

Use these error values:

```go
ErrInvalidRequest
ErrForbiddenTemplate
ErrIdempotencyConflict
ErrRateLimited
ErrNotFound
ErrNotificationsDisabled
```

- [ ] **Step 4: Implement PostgreSQL throttling**

Rate-limit keys are HMACs over caller, template, target hash, and window. No raw email appears in keys or logs. Return a retry duration with `ErrRateLimited`.

- [ ] **Step 5: Verify and commit**

```bash
go test ./internal/service ./internal/store -count=1
go test ./...
go vet ./...
git add internal/service internal/store internal/notify/service.go internal/notify/types.go
git commit -m "feat: persist idempotent notification intents"
```

### Task 5: Add Dapr-Only HTTP API And Readiness

**Files:**
- Create: `internal/httpapi/handler.go`
- Create: `internal/httpapi/handler_test.go`
- Delete: `internal/notify/handler.go`
- Delete: `internal/notify/handler_test.go`

**Interfaces:**
- Consumes: `service.Send`, `service.Get`, DB ping, allowed caller config.
- Produces: the four documented routes.

- [ ] **Step 1: Write failing HTTP tests**

Cover:

```text
production request without Dapr-Caller-App-Id returns 401
unknown caller returns 403
dev caller header works only when explicitly enabled
missing Idempotency-Key returns 400
idempotency conflict returns 409
rate limit returns 429 and Retry-After
same caller can read status
different caller receives 404
/health is process-only
/ready returns 503 when DB ping fails
```

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/httpapi -count=1
```

- [ ] **Step 3: Implement caller identity matching Asset API**

Trust `Dapr-Caller-App-Id` only. Permit `X-HHC-Caller-App-Id` only when `NOTIFICATION_ALLOW_DEV_CALLER_HEADER=true`. Remove shared-token authentication.

- [ ] **Step 4: Implement bounded JSON and response envelopes**

Use `http.MaxBytesReader` at 64 KiB, reject unknown JSON fields, set request IDs, and return stable machine-readable error codes.

- [ ] **Step 5: Verify and commit**

```bash
go test ./internal/httpapi -count=1
go test ./...
git add internal/httpapi internal/notify/handler.go internal/notify/handler_test.go
git commit -m "feat: expose private notification intent api"
```

### Task 6: Add Typed Email Provider

**Files:**
- Create: `internal/providers/provider.go`
- Create: `internal/providers/smtp.go`
- Create: `internal/providers/smtp_test.go`
- Delete: `internal/notify/sender.go`
- Delete: `internal/notify/sender_test.go`

**Interfaces:**
- Consumes: decrypted validated delivery payload.
- Produces: `ProviderReceipt` and typed `ProviderError`.

- [ ] **Step 1: Write failing provider tests**

Cover accepted receipt, context cancellation, timeout, temporary SMTP response, permanent recipient rejection, and redacted logs.

- [ ] **Step 2: Implement the provider contract**

```go
type ErrorKind string

const (
	ErrorTemporary       ErrorKind = "temporary"
	ErrorPermanent       ErrorKind = "permanent"
	ErrorInvalidEndpoint ErrorKind = "invalid_endpoint"
	ErrorSuppressed      ErrorKind = "suppressed"
	ErrorRateLimited     ErrorKind = "rate_limited"
)
```

Use an explicit `net.Dialer`, connection deadlines, and SMTP commands rather than unbounded `smtp.SendMail`. Never log recipient addresses, bodies, reset links, or verification links.

- [ ] **Step 3: Verify and commit**

```bash
go test ./internal/providers -count=1
go test ./...
git add internal/providers internal/notify/sender.go internal/notify/sender_test.go
git commit -m "feat: classify notification provider delivery"
```

### Task 7: Implement Transactional Outbox And Service Bus

**Files:**
- Create: `internal/queue/queue.go`
- Create: `internal/queue/servicebus.go`
- Create: `internal/queue/servicebus_test.go`
- Create: `internal/outbox/dispatcher.go`
- Create: `internal/outbox/dispatcher_test.go`
- Delete: `internal/notify/memory_queue.go`
- Delete: `internal/notify/servicebus_queue.go`

**Interfaces:**
- Consumes: pending outbox rows.
- Produces: Service Bus messages containing only `deliveryId`.

- [ ] **Step 1: Write failing outbox tests**

Cover multi-replica lease exclusion, lease expiry recovery, stable Service Bus `MessageID`, publish retry with backoff, and marking an outbox row published only after broker acknowledgement.

- [ ] **Step 2: Implement Service Bus authentication**

- Production: `azidentity.NewDefaultAzureCredential` and `azservicebus.NewClient(namespaceFQDN, credential, nil)`.
- Development: allow `SERVICEBUS_CONNECTION_STRING` for the official emulator.
- Queue messages contain JSON `{"deliveryId":"uuid"}` and set broker
  `MessageID` to the outbox row UUID. Transport retries reuse that ID;
  application retries create a new outbox row and therefore a new ID.

- [ ] **Step 3: Implement leased outbox dispatch**

Claim due rows with `FOR UPDATE SKIP LOCKED`, set a 60-second lease, publish, then mark published. Use bounded exponential backoff with jitter and stop the process if a persistent DB or queue connection failure makes progress impossible.

- [ ] **Step 4: Verify and commit**

```bash
go test ./internal/queue ./internal/outbox -count=1
go test ./...
go vet ./...
git add internal/queue internal/outbox internal/notify/memory_queue.go internal/notify/servicebus_queue.go
git commit -m "feat: publish notification deliveries through service bus"
```

### Task 8: Add Idempotent Delivery Worker And Retention

**Files:**
- Create: `internal/worker/worker.go`
- Create: `internal/worker/worker_test.go`
- Create: `internal/retention/worker.go`
- Create: `internal/retention/worker_test.go`
- Delete: `internal/notify/worker.go`

**Interfaces:**
- Consumes: Service Bus delivery IDs and provider.
- Produces: durable delivery transitions and broker settlement decisions.

- [ ] **Step 1: Write failing worker tests**

Cover:

```text
already sent delivery completes without a second provider call
two workers cannot send one leased delivery concurrently
expired lease is recoverable
temporary failure schedules retry and completes current broker message
permanent failure marks failed and dead-letters broker message
retry exhaustion marks dead_lettered
provider success stores receipt before completing broker message
shutdown finishes or releases in-flight work
```

- [ ] **Step 2: Implement lifecycle**

Use:

```text
queued -> sending -> sent
queued -> sending -> queued       transient retry
queued -> sending -> failed       permanent error
queued -> sending -> dead_lettered retry exhaustion
```

Do not abandon a broker message merely to schedule application backoff. Persist `next_attempt_at`, insert a new outbox row when due, and complete the current broker message.

- [ ] **Step 3: Implement retention**

The retention worker:

- Replaces target and payload ciphertext with empty tombstones seven days after terminal status.
- Keeps hashes, template version, status, attempt counts, timestamps, and provider receipt metadata for 730 days.
- Deletes expired rate-limit buckets.
- Uses leased batches and is safe across replicas.

- [ ] **Step 4: Verify and commit**

```bash
go test ./internal/worker ./internal/retention -count=1
go test ./...
go vet ./...
git add internal/worker internal/retention internal/notify/worker.go
git commit -m "feat: process durable notification deliveries"
```

### Task 9: Split API, Worker, And Migration Runtime Modes

**Files:**
- Create: `cmd/notification/main.go`
- Create: `cmd/notification/main_test.go`
- Delete: `cmd/server/main.go`
- Modify: `Dockerfile`
- Modify: `README.md`

**Interfaces:**
- Consumes: all runtime components.
- Produces: `/notification-api api`, `/notification-api worker`, `/notification-api migrate`.

- [ ] **Step 1: Write failing runtime tests**

Verify invalid mode fails, API starts HTTP plus outbox/retention loops, worker starts consumer plus readiness HTTP, migration mode runs once and exits, and any required background loop failure cancels the process.

- [ ] **Step 2: Implement one binary with explicit subcommands**

```text
notification-api api
notification-api worker
notification-api migrate
```

The API runs HTTP, outbox dispatch, and retention. The worker runs Service Bus receive and a private health server. Both use signal-aware graceful shutdown.

- [ ] **Step 3: Update distroless image**

Build `./cmd/notification`, use a non-root distroless runtime, expose 8081, and leave the default command as `api`.

- [ ] **Step 4: Verify local runtime**

```bash
go test ./...
go vet ./...
docker build -t notification-api:local .
```

- [ ] **Step 5: Commit**

```bash
git add cmd Dockerfile README.md
git commit -m "build: split notification api and worker modes"
```

### Task 10: Cut Account API To The Canonical Contract

**Repo:** `/Users/rayselfs/Projects/hhc/account/account-api`

**Files:**
- Modify: `internal/services/mail_service.go`
- Modify: `internal/services/mail_service_test.go`
- Modify: `.env.example`

**Interfaces:**
- Consumes: canonical send route and Dapr invocation.
- Produces: stable idempotency keys for verification and reset operations.

- [ ] **Step 1: Rewrite failing client tests**

Expected path:

```text
http://localhost:3500/v1.0/invoke/notification-api/method/priv/notifications/send
```

Expected template IDs:

```text
account.verify-email
account.reset-password
```

The operation token digest is part of the idempotency key but never logged.

- [ ] **Step 2: Run focused tests**

```bash
go test ./internal/services -run MailService -count=1
```

- [ ] **Step 3: Implement direct cutover**

No legacy adapter is required because neither service is deployed. Remove `NOTIFICATION_INTERNAL_TOKEN`; use Dapr invocation and preserve request timeout.

- [ ] **Step 4: Verify and commit**

```bash
go test ./internal/services -run MailService -count=1
go test ./...
go vet ./...
git add .env.example internal/services/mail_service.go internal/services/mail_service_test.go
git commit -m "feat: use durable notification intents"
```

### Task 11: Create PostgreSQL Database And Azure Infrastructure

**Files:**
- Create: `infra/main.bicep`
- Create: `infra/main.bicepparam.example`
- Create: `infra/README.md`
- Create: `scripts/bootstrap-database.sh`
- Create: `scripts/bootstrap-database.test.sh`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: existing `alive-env`, `alive` ACR, `alive-vault`, and private PostgreSQL at `172.16.68.4`.
- Produces: notification DB, Service Bus queue, API ACA, worker ACA, migration job, managed identities, roles, probes, and KEDA rule.

- [ ] **Step 1: Write bootstrap script self-test**

The test runs the script in dry-run mode and verifies it:

- Reads only `PG_ADMIN_PASSWORD` from the ignored JSON.
- Generates a 48-character hex notification password when absent.
- Never prints either password.
- Targets host `172.16.68.4`, database `notification`, role `notification`, and `sslmode=require`.
- Leaves `/Users/rayselfs/Projects/hhc/.env.json` at mode `0600`.

- [ ] **Step 2: Implement idempotent DB bootstrap**

The script executes:

```sql
CREATE ROLE notification LOGIN PASSWORD :'notification_password';
CREATE DATABASE notification OWNER notification;
\connect notification
ALTER SCHEMA public OWNER TO notification;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO notification;
```

Use catalog checks and `\gexec` so rerunning updates the role password without recreating the database. Store this URL in Key Vault secret `notification-database-url`:

```text
postgres://notification:<encoded-password>@172.16.68.4:5432/notification?sslmode=require
```

- [ ] **Step 3: Create Bicep resources**

Use:

```text
Service Bus Standard namespace: alive-notifications-${uniqueString(subscription().id, resourceGroup().id)}
Queue: notifications-email
Duplicate detection: enabled, 10-minute history
Lock duration: 2 minutes
Max delivery count: 10
Default TTL: 7 days
Dead-letter on expiration: enabled
API ACA: notification-api, Dapr app-id notification-api, min 1, max 3, no ingress
Worker ACA: notification-worker, min 0, max 5, no ingress
Migration job: notification-migrate
```

Assign API identity `Azure Service Bus Data Sender`, worker identity `Azure Service Bus Data Receiver`, worker KEDA identity, ACR pull identities, and Key Vault Secrets User for required secrets.

- [ ] **Step 4: Add probes and scaling**

- API liveness `/health`, readiness `/ready`, port 8081.
- Worker liveness/readiness on its private health port.
- Worker KEDA type `azure-servicebus`, queue `notifications-email`, `messageCount=5`, managed identity.

- [ ] **Step 5: Validate infrastructure**

```bash
bash scripts/bootstrap-database.test.sh
az bicep build --file infra/main.bicep
az deployment group what-if -g alive -f infra/main.bicep -p infra/main.bicepparam.example
```

The parameter example contains no secrets.

- [ ] **Step 6: Commit**

```bash
git add .gitignore infra scripts
git commit -m "build: provision notification service infrastructure"
```

### Task 12: Add GitHub Actions Release Pipeline

**Files:**
- Create: `.github/workflows/release.yml`
- Modify: `infra/README.md`

**Interfaces:**
- Consumes: Bicep resources, GitHub OIDC variables, ACR.
- Produces: tested image, migration, API/worker rollout, readiness verification.

- [ ] **Step 1: Add workflow validation**

The workflow triggers on `main` changes under Go source, migrations, Dockerfile, infra, or the workflow. It uses:

```text
permissions: contents:read, id-token:write
concurrency: notification-production, cancel-in-progress:false
image tag: main-${GITHUB_SHA::7}
```

The verify job starts a PostgreSQL service container, creates
`notification_test`, sets `TEST_DATABASE_URL`, and runs both:

```bash
go test ./...
go test -tags=integration ./... -count=1
```

- [ ] **Step 2: Implement release order**

```text
go test ./...
go vet ./...
Azure OIDC login
az acr build with immutable SHA tag
Bicep deployment/update
start notification-migrate and require success
update API and worker images
wait API and worker latest revisions ready
Dapr invoke notification-api /ready from api-gateway
```

Do not deploy API/worker if migration fails.

- [ ] **Step 3: Bind GitHub OIDC**

Reuse the current HHC deployment application only with an immutable repository federated credential for `HallelujahHomeChurch/notification-api` main. Set repository variables:

```text
AZURE_CLIENT_ID
AZURE_TENANT_ID
AZURE_SUBSCRIPTION_ID
```

No Azure credentials or DB/provider secrets are stored in GitHub.

- [ ] **Step 4: Validate workflow and commit**

```bash
ruby -e 'require "yaml"; YAML.load_file(".github/workflows/release.yml"); puts "workflow yaml ok"'
git diff --check
git add .github/workflows/release.yml infra/README.md
git commit -m "ci: deploy notification api and worker"
```

### Task 13: Production Verification And Runbook

**Files:**
- Create: `docs/runbook.md`
- Modify: `README.md`

**Interfaces:**
- Consumes: deployed API, worker, DB, Service Bus, provider.
- Produces: repeatable acceptance, incident, replay, rollback, and DR procedures.

- [ ] **Step 1: Add integration tests**

Run against staging:

```text
unauthorized Dapr caller is rejected
account-api can enqueue verification and reset intents
same idempotency request produces one provider send
conflicting idempotency request returns 409
worker restart recovers an expired lease
temporary provider outage retries with backoff
permanent failure becomes failed
retry exhaustion appears in DB and Service Bus DLQ
API remains available while worker scales to zero
disabled kill switch accepts no new sends
logs contain no email, token, or URL
```

- [ ] **Step 2: Document operations**

Include:

- Queue pause/resume.
- `NOTIFICATIONS_DISABLED` kill switch.
- DLQ inspect and replay by delivery ID.
- Provider credential rotation.
- DB backup/restore.
- Rollback to previous image without rolling back migrations.
- DR rule: restored environments start with sending disabled until ledger, outbox, queue, and provider receipts are reconciled.
- Alerts for oldest pending age, DLQ count, provider failure rate, outbox backlog, worker replicas, and readiness.

- [ ] **Step 3: Final verification**

```bash
go test ./...
go vet ./...
docker build -t notification-api:verify .
git diff --check
```

Then verify:

```bash
az containerapp show -g alive -n notification-api
az containerapp show -g alive -n notification-worker
az containerapp job show -g alive -n notification-migrate
namespace="$(az servicebus namespace list -g alive --query "[?starts_with(name, 'alive-notifications-')].name | [0]" -o tsv)"
az servicebus queue show -g alive --namespace-name "${namespace}" -n notifications-email
```

- [ ] **Step 4: Commit**

```bash
git add README.md docs/runbook.md
git commit -m "docs: add notification production runbook"
```

## Release Gate

Do not begin production deployment of `account-api` until all conditions are true:

- Notification database exists and migrations pass.
- Production API rejects requests without a trusted Dapr caller.
- Idempotency, encryption, status, retry, DLQ, and retention tests pass.
- Real verification and password-reset emails arrive without secrets appearing in logs.
- API and worker revisions are ready and Dapr invocation succeeds.
- GitHub Actions main release is green.
- DLQ replay and notification kill-switch runbooks have been exercised once.

## Deferred Capabilities

The following are intentionally absent from this implementation:

- Web Push subscription registration and VAPID provider.
- FCM/APNs installation registration and provider credentials.
- LINE, SMS, and admin in-app transports.
- Runtime template CMS.
- Bulk campaign, newsletter, scheduler, and recipient preference center.
- Multi-provider automatic failover.
- Premium Service Bus Geo-Replication.

The message/delivery split, channel text field, endpoint reference, provider interface, versioned template registry, and provider-neutral status model are the only v1 accommodations for those future capabilities.
