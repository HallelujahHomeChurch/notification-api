# Notification data-governance scope

This inventory describes existing metadata and behavior only. It exports no database values, changes no runtime code, enables no policy, changes no notification or DSR setting, and makes no legal-basis decision.

## Deterministic Account attribution

- The private DSR routes accept only the exact `account-api` caller. That trust boundary supplies the verified canonical email used for lookup; `notification-api` lowercases, trims and validates email syntax, but syntax validation alone does not verify an Account identity.
- DSR lookup normalizes that email and calculates candidate HMAC values across every configured retained hash key. Only matching email rows with the persisted `hash_key_id + target_hash` pair are deterministic. Delivery and outbox rows inherit that attribution only through their enforced foreign-key joins to the matched message. Web Push targets hash subscription JSON rather than email and remain manual scope.
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

DSR is separate from age-based retention and is not encoded as an unsupported duration or expiry rule. The private Account caller supplies request/user identifiers and a verified canonical email. DSR userId is validation/correlation only and is not queried; current DSR lookup uses retained key IDs and email-derived hashes. Notification normalizes the email, searches HMACs across retained hash keys, and exports only bounded message/delivery metadata. Erasure waits for claimed provider calls, clears target and payload ciphertext, replaces `target_hash` with a fixed tombstone, sets `payload_purged_at`, preserves message/delivery receipt metadata, and makes subsequent email lookup return no rows. Restrict-processing reports `not_applicable` without mutation.

Behavioral references:

- `internal/dsr/service_test.go`: `TestExportFindsMessagesAcrossRetainedHashKeys`, `TestExportReturnsMetadataWithoutCiphertextOrProviderIdentifiers`, `TestEraseTombstonesAttributedPayloadAndBreaksEmailLookup`, `TestRestrictProcessingReturnsNotApplicableWithoutMutation`.
- `internal/integration/postgres_test.go`: `TestPostgresDSRExportAndEraseAcrossHashRotation`.
- `internal/worker/worker_integration_test.go`: `TestPostgresEraseWaitsForClaimedProviderCall`, `TestPostgresEraseRollbackLeavesQueuedDeliveryRetryable`.

## Encrypted writer shapes

- Email target plaintext is the normalized canonical address.
- Web Push target plaintext is normalized JSON with exactly `endpoint`, `keys.p256dh`, and `keys.auth`.
- Encrypted payload plaintext contains `locale` separately from `fields`. Current registry fields are: Account verification `verifyUrl`; password reset `resetUrl`; OAuth link confirmation `confirmUrl` and `provider`; OAuth onboarding `code` and `provider`; newsletter `subject`, `body`, `actionUrl`, `unsubscribeUrl`, and `oneClickUnsubscribeUrl`; Web Push `title`, `body`, `clickBehavior`, and `actionUrl`.
- `template_id`, `template_version`, `channel`, `target_type`, `resource_type`, and `resource_id` are retained message metadata, not encrypted payload fields.
