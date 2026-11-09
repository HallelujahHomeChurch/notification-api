# Exchange Online readiness

Source baseline `a387ac4625ff44ad8ec929c69d4bbf2b4a4978ac` of `notification-api` on 2026-11-10. This inventory records local source. The live readiness fields below remain open.

| Area | Source evidence | Required before cutover |
| --- | --- | --- |
| Provider and revision | `EMAIL_PROVIDER` defaults to SMTP and can select SES. API admission pins the provider. The worker rejects a mismatched pin. Accepted attempts recover their durable receipt. | Read back active API and worker revisions, image digests, provider selections, compatible rollback images and actual SMTP/SES use. |
| Email and Web Push runtime | Separate API and persistent worker use a shared Service Bus queue. The worker manifest uses 0.25 vCPU, 0.5 GiB, 1–5 replicas and 30-second polling. Migration is a separate manual job with a dedicated DB credential reference. | Record effective runtime overrides, queue ownership, active and dead-letter counts, queued/in-flight pins and attempts by outcome. Reconcile pending Job safeguards with the existing owner. |
| Legacy limits and retention | Defaults are 20 attempts/min and 100/hour, with transactional reserves of 5/min and 20/hour. Quota delays use the outbox and preserve expiry. Migration 007 adds receipt evidence and 008 adds technical suppression and SES feedback. | Verify current ACS limits and other senders sharing the quota. Capture existing ACS/SES resources, identities, Key Vault references and feedback owners without credential values. Preserve ACS hourly enforcement for eligible ACS pins until retirement. |
| Exchange sender and rights | Graph has no runtime wiring at this baseline. | Verify mailbox immutable ID, accepted sending domain, SPF/DKIM/DMARC, applicable license and assignment terms. Record authorized mailbox and DNS owners. Check scoped worker submission and separate scoped feedback reader, including effective permissions. |
| Capacity and feedback | Current shared budget is for legacy providers. SES feedback integration is present in source. | Confirm TERRL, mailbox limits, other tenant senders, 30-day aggregate daily and peak volumes, and approved daily external-recipient budget and reserve. Verify Graph feedback durability and correlation before allowing bulk. |
| Acceptance and authorization | The service contracts keep Account transactional calls and Engagement campaign, status and eligibility calls provider-neutral. N0/N1 are source and local test work. | Capture compatible consumer builds and a reviewed rollout/rollback preview. Record approved test recipients and purpose, acceptable duplicate/unknown handling, responsible operators and explicit cutover authority. |

The Notification refactor owns the provider boundary and delivery evidence. Engagement's preflight and consent ordering work stays with Engagement and Account. The existing send-time eligibility check remains authoritative. Workload identity changes and runtime ownership changes require their own owner reviews.

Existing drafts remain separate. #63 owns optional queue isolation and fake capacity verification. #97 owns Job database-role enforcement. #36 owns service notification transports and #43 owns inbox delivery. Their proposal contents do not establish the runtime baseline.

Use the [cutover gates](../runbook.md#exchange-online-cutover-gates) when the pending fields have evidence and an approved operational scope.
