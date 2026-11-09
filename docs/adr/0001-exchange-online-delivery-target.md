# Exchange Online is the target for notification email

Accepted as the delivery direction on 2026-11-10. Runtime activation requires a separate approved change. This decision supersedes the workspace ADR that selected AWS SES as the migration target.

Notification continues to own durable delivery intents, attempts, receipts and provider feedback. Account owns login and recovery policy. Engagement owns campaign consent, audience, and eligibility. Web Push remains on the existing transport.

The target transport is Microsoft Graph app-only submission from a dedicated Exchange Online mailbox. Pin the mailbox by its immutable ID and scope the named worker managed identity to that mailbox. Check for equivalent tenant-wide permissions that would expand that scope. Use a separately scoped feedback reader. The API process needs no mail submission permission.

Provider acceptance and inbox delivery are separate evidence. Persist accepted submissions before settlement, reconcile feedback using durable correlation, and keep historic receipts readable. Bulk unknowns stop for reconciliation. Transactional unknowns retain bounded, same-provider retries under the original expiry and idempotency rules. Accepted attempts are never resubmitted, and unknown attempts never fail over across providers.

The existing SMTP route and SES implementation remain available under their current policy. The ACS hourly gate applies while ACS-pinned work can still send. Historical SES/SMTP receipt readers remain supported. SES operation or retirement, mailbox/licensing changes, and login verification, recovery and device notices require their respective acceptance and authorization.

See the [readiness record](../reviews/2026-11-10-notification-exchange-readiness.md) and [cutover gates](../runbook.md#exchange-online-cutover-gates).
