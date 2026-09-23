# Notification data-governance scope

This inventory describes existing metadata and behavior only. It exports no database values, changes no runtime code, enables no policy, changes no notification or DSR setting, and makes no legal-basis decision.

## Deterministic Account attribution

- The private DSR routes accept only the exact `account-api` caller. That trust boundary supplies the verified canonical email used for lookup; `notification-api` lowercases, trims and validates email syntax, but syntax validation alone does not verify an Account identity.
- Trusted Account and Engagement senders may supply `subjectAccountId`. Notification canonicalizes the UUID and stores only `subject_hash_key_id + subject_hash`, computed across its own HMAC key ring; this makes new Email and Web Push rows deterministic without storing the Account ID. DSR cleanup also normalizes current Email and calculates legacy target hashes across retained keys. Delivery and outbox rows inherit attribution through enforced foreign-key joins.
- `request_hash`, `target_hash`, `bucket_key`, ciphertext and provider identifiers are pseudonymous or personal data, not anonymous data. Generic `resource_type` and `resource_id` values remain manual scope until a tested Account predicate exists; they are not deterministic joins today.

## Explicit manual and excluded boundaries

- `notification_deliveries.endpoint_ref` has no located writer at this pinned source revision and remains optional/manual.
- Rate-bucket HMACs are not enumerable Account-subject joins and must not be used to infer identity.
- Provider-held email and Web Push data, plus broker-side Service Bus retention, are external/manual boundaries. This manifest inventories only Notification-owned PostgreSQL rows.
- Backups, logs and provider/broker deletion operations remain governed by their own operational procedures; no unimplemented cleanup is claimed here.

## Operational schema exclusions

- `schema_migrations.version` — Migration filename, not an Account subject.
- `schema_migrations.checksum` — Migration content checksum, not an Account subject.
- `schema_migrations.applied_at` — Schema deployment time, not an Account event.

## Existing age-based retention

- At `terminal_at <= now-7d`, the existing worker clears only `target_ciphertext` and `payload_ciphertext` and sets `payload_purged_at`; message metadata, delivery receipts and outbox state remain.
- At `terminal_at <= now-730d`, the worker deletes the parent message. PostgreSQL cascades that deletion to its delivery and outbox rows.
- `notification_rate_limits` rows are deleted only when `expires_at <= now`; active buckets are preserved.
- `expires_at` on a notification message bounds provider-delivery eligibility. It is not the message-row retention trigger.
- Concurrency uses bounded batches and `FOR UPDATE SKIP LOCKED`. Repeated runs preserve ineligible rows and return zero work after eligible rows are handled.

These boundaries are characterized by `TestRunOnceUsesRequiredRetentionWindowsAndBoundedBatch`, `TestPostgresRetentionTombstonesThenDeletesWithoutLosingReceiptMetadata`, `TestRetentionPreservesRecentAndNonterminalPayloadsOnRepeat`, and `TestPostgresRetentionIsSafeAcrossReplicas`.

## Existing request-triggered DSR behavior

DSR is separate from age-based retention and is not encoded as an unsupported duration or expiry rule. The private Account caller supplies request/user identifiers and a verified canonical email. DSR userId is canonicalized and queried only through retained-key subject HMACs; current email remains a legacy fallback lookup. Unattributed historical rows are not inferred from payload or generic resource text and are surfaced as `legacy_unattributed_notifications`. Erasure waits for claimed provider calls, clears target and payload ciphertext plus direct subject hashes, replaces `target_hash` with a fixed tombstone, sets `payload_purged_at`, preserves message/delivery receipt metadata, and makes subsequent deterministic lookup return no rows. Restrict-processing reports `not_applicable` without mutation.

Completed Account cleanup also retains a bounded idempotency tombstone: the caller key, a domain-separated SHA-256 reference to the canonical Account UUID, result counts/status/reason codes, and timestamps. It retains neither the UUID nor email. This prevents a completed destructive operation from being replayed for another subject or the deleted account from being silently recreated; its retention period remains `pending_legal` and no unsupported cleanup is claimed.

Behavioral references:

- `internal/dsr/service_test.go`: `TestExportFindsMessagesAcrossRetainedHashKeys`, `TestExportReturnsMetadataWithoutCiphertextOrProviderIdentifiers`, `TestEraseTombstonesAttributedPayloadAndBreaksEmailLookup`, `TestRestrictProcessingReturnsNotApplicableWithoutMutation`.
- `internal/integration/postgres_test.go`: `TestPostgresDSRExportAndEraseAcrossHashRotation`.
- `internal/worker/worker_integration_test.go`: `TestPostgresEraseWaitsForClaimedProviderCall`, `TestPostgresEraseRollbackLeavesQueuedDeliveryRetryable`.

## Encrypted writer shapes

- Email target plaintext is the normalized canonical address.
- Web Push target plaintext is normalized JSON with exactly `endpoint`, `keys.p256dh`, and `keys.auth`.
- Encrypted payload plaintext contains `locale` separately from `fields`. Current registry fields are: Account verification `verifyUrl`; password reset `resetUrl`; OAuth link confirmation `confirmUrl` and `provider`; OAuth onboarding `code` and `provider`; newsletter `subject`, `body`, `actionUrl`, `unsubscribeUrl`, and `oneClickUnsubscribeUrl`; Web Push `title`, `body`, `clickBehavior`, and `actionUrl`.
- `template_id`, `template_version`, `channel`, `target_type`, `resource_type`, and `resource_id` are retained message metadata, not encrypted payload fields.
