# Notification Production Runbook

This runbook covers the current `notification-api`, `notification-worker`,
PostgreSQL ledger/outbox, Azure Service Bus queue, Key Vault secrets, and SMTP
provider. It does not record a completed production acceptance. See
[Production acceptance](#production-acceptance) for current status.

Delivery is at-least-once. Each delivery keeps one stable RFC `Message-ID`
across retries so downstream systems can best-effort deduplicate it, but SMTP
cannot atomically commit provider acceptance with the PostgreSQL ledger. A
lost final SMTP response is recorded as `acceptance_unknown` and retried to
avoid silently dropping account verification or password-reset email.

## Resources and safety rules

- Resource group: `alive`
- API Container App: `notification-api`
- Worker Container App: `notification-worker`
- Migration job: `notification-migrate`
- Queue: `notifications-email`
- Key Vault: `alive-vault`
- Immutable image format:
  `alive.azurecr.io/alive/notification-api:main-<short-sha>`

Before changing production:

1. Freeze notification releases and record the incident/change identifier.
2. Record current API/worker image and latest ready revision.
3. Never print `DATABASE_URL`, SMTP credentials, encryption/hash keys, email
   addresses, decrypted payloads, reset/verification URLs, or tokens.
4. Never replay an ambiguous delivery until provider-side evidence shows it
   was not accepted.
5. Use forward-only migrations. Never down-migrate production.

Discover the generated Service Bus namespace:

```bash
resource_group=alive
queue_name=notifications-email
namespace="$(
  az servicebus namespace list \
    --resource-group "${resource_group}" \
    --query "[?starts_with(name, 'alive-notifications-')].name | [0]" \
    --output tsv
)"
test -n "${namespace}"
```

## Incident triage

1. Confirm the affected template, time window, and whether the symptom is
   rejection, backlog, duplicate risk, or provider failure. Do not collect
   recipient addresses in the incident channel.
2. Check API and worker latest/ready revisions and immutable images.
3. Check API and worker readiness. `/ready` only proves PostgreSQL
   connectivity; it does not prove Service Bus or SMTP availability.
4. Check queue status, active-message count, and DLQ count.
5. Run the non-PII [database health queries](#database-health-queries).
6. Check SMTP error kind/operation and provider-side acceptance records.
7. Choose the narrowest containment:
   - reject only new intents with `NOTIFICATIONS_DISABLED`;
   - pause delivery with queue `ReceiveDisabled`;
   - for replay or recovery, pause the queue and deactivate the worker
     revision.
8. Reconcile provider acceptance before replaying any `sending`, `failed`, or
   `dead_lettered` delivery.

## Pause and resume

### Pause queue consumption

`ReceiveDisabled` stops receives but still permits the API outbox to publish.
It is the delivery pause control; it does not reject new notification intents.

```bash
az servicebus queue update \
  --resource-group "${resource_group}" \
  --namespace-name "${namespace}" \
  --name "${queue_name}" \
  --status ReceiveDisabled \
  --output none

az servicebus queue show \
  --resource-group "${resource_group}" \
  --namespace-name "${namespace}" \
  --name "${queue_name}" \
  --query status \
  --output tsv
```

For maintenance requiring a stopped worker, save and deactivate its current
revision after setting `ReceiveDisabled`:

```bash
worker_revision="$(
  az containerapp show \
    --resource-group alive \
    --name notification-worker \
    --query properties.latestRevisionName \
    --output tsv
)"
az containerapp revision deactivate \
  --resource-group alive \
  --name notification-worker \
  --revision "${worker_revision}"
```

Do not run a release while the worker is intentionally deactivated.

### Resume queue consumption

Set the queue to `Active`, activate the saved worker revision, and watch queue
depth plus worker readiness:

```bash
az servicebus queue update \
  --resource-group "${resource_group}" \
  --namespace-name "${namespace}" \
  --name "${queue_name}" \
  --status Active \
  --output none

az containerapp revision activate \
  --resource-group alive \
  --name notification-worker \
  --revision "${worker_revision}"
```

### Reject new intents

`NOTIFICATIONS_DISABLED=true` makes `POST /priv/notifications/send` return
`503 NTF_DISABLED`. The check occurs before a new ledger intent is created.
Existing ledger rows, outbox publishing, queued deliveries, and worker
processing continue.

```bash
az containerapp update \
  --resource-group alive \
  --name notification-api \
  --set-env-vars NOTIFICATIONS_DISABLED=true \
  --output none

api_revision="$(
  az containerapp show \
    --resource-group alive \
    --name notification-api \
    --query properties.latestRevisionName \
    --output tsv
)"
az containerapp revision show \
  --resource-group alive \
  --name notification-api \
  --revision "${api_revision}" \
  --query 'properties.{a:active,h:healthState,p:provisioningState}' \
  --output yaml
```

Do not declare containment until this exact revision is active, healthy, and
provisioned. Verify its effective container environment contains
`NOTIFICATIONS_DISABLED=true`, then call `/priv/notifications/send` through an
allowlisted Dapr caller (`account-api` or `hhc-web-api`) and require
`503 NTF_DISABLED`. Do not use a direct ingress request or spoof
`Dapr-Caller-App-Id`.

This is not a full delivery stop. Use `ReceiveDisabled` when existing
notifications must not reach SMTP. The production release refuses to deploy
while this emergency override is `true`; re-enable it explicitly only after the
incident is resolved.

Re-enable new intents explicitly:

```bash
az containerapp update \
  --resource-group alive \
  --name notification-api \
  --set-env-vars NOTIFICATIONS_DISABLED=false \
  --output none
```

Repeat the exact revision readiness and effective-environment checks. Send one
approved acceptance intent through an allowlisted Dapr caller before declaring
the service re-enabled.

## Credential and secret rotation

Container Apps reference versionless Key Vault secret URLs. Perform rotations
under a change window and restart the affected ready revision after the new
secret version is available.

### PostgreSQL credential

1. Set `NOTIFICATIONS_DISABLED=true`, set the queue to `ReceiveDisabled`, and
   deactivate the worker.
2. Rotate only the `notification` database role password on the shared server.
3. Build the new URL without logging it and write it to
   `notification-database-url` in `alive-vault`.
4. Restart the API revision and verify `/ready`.
5. Activate the worker revision and verify its readiness.
6. Resume the queue, then set `NOTIFICATIONS_DISABLED=false`.
7. Revoke the old credential if the database rotation mechanism allowed an
   overlap.

Never store the URL in shell history, CI variables, or the incident record.

### SMTP credential

1. Pause queue consumption.
2. Rotate the provider credential.
3. Update both `notification-smtp-username` and
   `notification-smtp-password` in `alive-vault`.
4. Restart the worker revision.
5. Set the queue to `Active` for a controlled acceptance, send one approved
   non-production recipient message, and inspect redacted logs.
6. Leave the queue active only when the acceptance succeeds; otherwise return
   it to `ReceiveDisabled`.

`/ready` does not connect to SMTP and cannot validate this rotation.

### Encryption and hash keys

The expand migration adds `encryption_key_id` and `hash_key_id` with the
non-null default `legacy-v1`. Before running it, configure the current
`notification-data-encryption-key` and `notification-hash-key` values under
`legacy-v1` in `NOTIFICATION_ENCRYPTION_KEYS_JSON` and
`NOTIFICATION_HASH_KEYS_JSON`, and set both active key IDs to `legacy-v1`.
The legacy single-key environment variables remain a supported upgrade
fallback. While old and versioned settings coexist, each legacy value must
match the corresponding `legacy-v1` keyring entry exactly; startup rejects a
mismatch so rolling replicas cannot write incompatible data under one key ID.

Do not activate another key ID until the API, store, and worker are all wired
to persist and read key IDs. The keyring-aware release persists active IDs and
reads historical IDs before another key is activated.
Replacing only the old Key Vault values is unsafe:

- the worker needs the current encryption key for every non-purged queued
  payload;
- changing the hash key changes stored request/target hashes, idempotent replay
  comparison, and rate-limit buckets.

Keep every referenced key configured until a preflight confirms no retained
row depends on it. API startup checks retained hash-key references; worker
startup checks non-purged encryption-key references. Keep a previous hash key
configured for at least the longest rate-limit window (currently 24 hours), so
accepted requests continue incrementing both old and new buckets before
retirement. After activating a new key, rollback only to a keyring-aware image.
Keep sending disabled during an emergency reconciliation. Do not improvise a
secret-only rotation.

Managed identities do not have stored credentials to rotate.

## DLQ inspection and safe replay

The Service Bus body contains only `{"deliveryId":"<uuid>"}`. Inspect the DLQ
with Azure Portal Service Bus Explorer in non-locking **Peek** mode. Do not use
automatic resubmit: the old body references a terminal delivery and the worker
will dead-letter it again.

Replay prerequisites:

1. Set the queue to `ReceiveDisabled`, deactivate `notification-worker`, and
   verify that no worker revision is active.
2. Confirm the DLQ body contains one valid delivery ID.
3. Confirm the old database delivery is `failed` or `dead_lettered`.
4. Confirm the payload has not been purged.
5. Reconcile SMTP/provider acceptance. If acceptance is unknown, do not replay.
6. Generate new UUIDv4 values for `new_delivery_id` and `new_outbox_id`.

Run the following through a controlled `psql` session. It does not select or
print recipient, payload, idempotency, or resource data:

```sql
\set ON_ERROR_STOP on
\set old_delivery_id '00000000-0000-4000-8000-000000000001'
\set new_delivery_id '00000000-0000-4000-8000-000000000002'
\set new_outbox_id '00000000-0000-4000-8000-000000000003'

BEGIN;

WITH source AS (
    SELECT old.message_id, old.channel, old.endpoint_ref, old.provider
    FROM notification_deliveries AS old
    JOIN notification_messages AS message ON message.id = old.message_id
    WHERE old.id = :'old_delivery_id'::uuid
      AND old.status IN ('failed', 'dead_lettered')
      AND message.payload_purged_at IS NULL
    FOR UPDATE OF old, message
),
new_delivery AS (
    INSERT INTO notification_deliveries (
        id, message_id, channel, endpoint_ref, provider, status
    )
    SELECT
        :'new_delivery_id'::uuid,
        message_id,
        channel,
        endpoint_ref,
        provider,
        'queued'
    FROM source
    RETURNING id, message_id
),
message_reset AS (
    UPDATE notification_messages AS message
    SET status = 'queued',
        terminal_at = NULL,
        updated_at = clock_timestamp()
    FROM new_delivery
    WHERE message.id = new_delivery.message_id
    RETURNING message.id AS message_id
)
INSERT INTO notification_outbox (id, delivery_id, status)
SELECT :'new_outbox_id'::uuid, new_delivery.id, 'pending'
FROM new_delivery
JOIN message_reset USING (message_id);

SELECT 1 / CASE WHEN (
    SELECT count(*) = 1
    FROM notification_outbox AS outbox
    JOIN notification_deliveries AS replay
      ON replay.id = outbox.delivery_id
    JOIN notification_deliveries AS old
      ON old.id = :'old_delivery_id'::uuid
    WHERE outbox.id = :'new_outbox_id'::uuid
      AND replay.id = :'new_delivery_id'::uuid
      AND replay.status = 'queued'
      AND old.status IN ('failed', 'dead_lettered')
) THEN 1 ELSE 0 END AS replay_guard;

COMMIT;
```

The old delivery remains terminal. The message row is the aggregate current
state and is reset for the new delivery. The API outbox publishes a new broker
message using the new outbox UUID, preserving duplicate-detection semantics.

After the transaction commits:

1. Confirm the new outbox becomes `published`.
2. Keep the worker inactive and set the queue to `Active`.
3. In Service Bus Explorer, receive the exact old DLQ message in PeekLock mode,
   verify its delivery ID again, and **Complete** it immediately. Do not hold
   the two-minute lock while running SQL or investigating provider state.
4. Activate the saved worker revision.
5. Confirm the new delivery reaches one terminal state.
6. If any step is ambiguous, pause again; never create another replay blindly.

## Database health queries

Run with a secure connection and without shell tracing. These queries return
only counts, statuses, and ages:

```sql
SELECT status, count(*)
FROM notification_messages
GROUP BY status
ORDER BY status;

SELECT
    count(*) AS due_outbox,
    COALESCE(
        max(EXTRACT(EPOCH FROM (
            clock_timestamp() -
            CASE
                WHEN status = 'pending' THEN next_attempt_at
                ELSE lease_expires_at
            END
        ))),
        0
    )::bigint AS oldest_due_seconds
FROM notification_outbox
WHERE (status = 'pending' AND next_attempt_at <= clock_timestamp())
   OR (status = 'publishing' AND lease_expires_at <= clock_timestamp());

SELECT
    count(*) AS due_deliveries,
    COALESCE(
        max(EXTRACT(EPOCH FROM (clock_timestamp() - next_attempt_at))),
        0
    )::bigint AS oldest_due_seconds
FROM notification_deliveries
WHERE status = 'queued'
  AND next_attempt_at <= clock_timestamp();

WITH terminal AS (
    SELECT
        count(*) FILTER (WHERE status = 'sent') AS sent,
        count(*) FILTER (
            WHERE status IN ('failed', 'dead_lettered')
        ) AS failed
    FROM notification_deliveries
    WHERE updated_at >= clock_timestamp() - interval '15 minutes'
      AND status IN ('sent', 'failed', 'dead_lettered')
)
SELECT
    sent,
    failed,
    CASE
        WHEN sent + failed = 0 THEN 0
        ELSE round(100.0 * failed / (sent + failed), 2)
    END AS failure_percent
FROM terminal;
```

`GET /health` is process liveness. `GET /ready` performs a PostgreSQL ping with
a two-second timeout. Both API and worker expose these paths on port `8081`;
neither readiness check validates Service Bus or SMTP.

## Initial alert thresholds

These are starting thresholds, not code-enforced SLOs. Re-baseline after live
traffic exists.

| Signal | Warning | Critical |
| --- | --- | --- |
| API readiness | 1 failure | 2 consecutive minutes |
| API rate limited | more than 10 HTTP 429s in 5 minutes | investigate request, recipient, and template volume |
| Oldest due outbox row | 120 seconds | 600 seconds |
| Due outbox rows | 100 for 5 minutes | 1,000 for 5 minutes |
| Oldest due delivery | 120 seconds | 600 seconds |
| Service Bus DLQ | 1 message | 10 messages or any sustained growth |
| Migration execution | n/a | Any non-`Succeeded` terminal result |

Provider terminal failure rate:

- warning: 5% over 15 minutes, with at least 20 terminal attempts;
- critical: 20% over 15 minutes, with at least 20 terminal attempts.

Evaluate worker readiness and replica count only when the queue is `Active`, an
active worker revision exists, the scaler is healthy, active message count is
greater than zero, and no approved maintenance window is open. Warn after two
minutes with zero replicas; page after five minutes.

Also alert on a non-`Active` queue outside an approved maintenance window and
on an API/worker latest revision that is not the latest ready revision.

The API enforces recipient limits of one message per 15 minutes and five per
24 hours, plus `NOTIFICATION_TEMPLATE_DAILY_LIMIT` per caller/template/day
(default `1000`). Tune the template-wide limit from observed legitimate volume;
do not remove it to accommodate a burst.

## Log redaction

Allowed operational identifiers are request ID, message ID, delivery ID,
outbox ID, template ID/version, state, attempt count, and provider error
kind/operation.

Never log:

- target email or SMTP envelope;
- decrypted subject/body or encrypted payload bytes;
- verification/reset URL or operation token;
- idempotency key or raw Service Bus body;
- database URL/password, SMTP username/password, or Key Vault secret value;
- encryption/hash key;
- PostgreSQL dump contents.

Do not enable shell tracing around secrets. Exported logs and SQL results must
be reviewed for these fields before attaching them to an incident.

## Immutable rollback

Rollback is allowed only when the previous binary is compatible with the
current forward schema. Prefer roll-forward when compatibility is uncertain.
Never delete `schema_migrations` rows or execute a down migration.

1. Select the exact previous `main-<short-sha>` image.
2. Deploy that image to `notification-migrate` with
   `deployRuntime=false provisionPermissions=false`.
3. Start one migration execution and require that exact execution to report
   `Succeeded`.
4. Only then deploy API and worker with
   `deployRuntime=true provisionPermissions=false`.
5. Require both latest ready revisions to use the exact rollback image and run
   the gateway Dapr `/ready` probe.

Use `infra/main.bicep` and the same parameter set/order as
`.github/workflows/release.yml`. Running the older image's migration command is
still forward-only; it does not undo migrations already applied by a newer
release.

## PostgreSQL PITR and disaster recovery

PostgreSQL is shared. Never point all clients at a restored server and never
restore the entire shared server over the source.

1. Keep the queue `ReceiveDisabled`, keep the worker deactivated, and set
   `NOTIFICATIONS_DISABLED=true`.
2. Restore the shared Azure PostgreSQL Flexible Server to a **new server** at
   the approved UTC restore time:

   ```bash
   az postgres flexible-server restore \
     --resource-group alive \
     --name <new-restore-server> \
     --source-server <shared-source-server> \
     --restore-time <ISO-8601-UTC>
   ```

3. From the new server, use `pg_dump --format=custom` for only the
   `notification` database. Store the dump as restricted sensitive data.
4. Create an empty recovery database owned by the `notification` role on the
   intended target server. Run `pg_restore --no-owner --no-acl` while connected
   as `notification`, or add `--role=notification` when the restore operator is
   allowed to `SET ROLE`. Do not overwrite another service database.
5. Verify that application schemas, tables, and sequences are owned by
   `notification`, and verify the role can select, insert, update, and use
   sequences before continuing.
6. Run forward notification migrations against the recovered database and
   execute the health queries.
7. Reconcile ledger, outbox, active queue, DLQ, and SMTP/provider acceptance
   records before changing `notification-database-url`.

Disaster-recovery sending remains disabled until all of the following are
resolved:

- every `sending` delivery is classified using provider evidence;
- pending/publishing outbox rows are matched to queue state;
- active and dead-letter queue delivery IDs are matched to database rows;
- any provider-accepted delivery is marked so it cannot be replayed;
- the restored database uses the matching encryption key.

`NOTIFICATIONS_DISABLED` alone does not stop existing work. Keep the queue
`ReceiveDisabled` and the worker deactivated throughout reconciliation. The
stored SMTP message ID is HHC's stable Internet `Message-ID`, not a provider
acceptance receipt. Use provider logs and the controlled time window; unknown
acceptance means no manual replay.

## Production acceptance

Status: **runtime and Azure Communication Services SMTP deployment accepted;
operational drills remain**. Static/local checks are not live acceptance.

- [x] Unit tests, PostgreSQL integration tests, and vet pass.
- [x] Bicep and release workflow static validation pass.
- [x] Migration-first workflow and immutable image readiness gates exist.
- [x] `SMTP_ADDR`, `SMTP_FROM`, and
      `SMTP_AUTHENTICATION_ENABLED` production repository variables are set.
- [x] Required SMTP Key Vault credentials exist when authentication is enabled.
- [x] Production migration job succeeds.
- [x] Production API and worker latest revisions become ready on the exact
      immutable image.
- [x] Gateway Dapr invocation of notification `/ready` returns the expected
      HTTP status and body.
- [x] Unauthorized Dapr callers are rejected in the deployed environment.
- [x] Real verification and password-reset emails are accepted by the
      production provider for an approved test recipient.
- [x] Logs are reviewed and contain no target, token, URL, payload, or secret.
- [ ] Retry, permanent failure, and DLQ behavior are exercised against the
      deployed provider.
- [ ] Queue pause/resume and `NOTIFICATIONS_DISABLED` are exercised.
- [ ] One safe DLQ replay drill is completed without duplicate provider send.
- [ ] Alerts and ownership are configured and tested.

Do not begin the production `account-api` cutover until every unchecked item is
completed and recorded.
