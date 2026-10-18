# Email throttle and replay safety implementation plan

> **For agentic workers:** Use superpowers:executing-plans inline. User approved implementation and PR creation; no merge, deployment, production configuration/data mutation, or replay.

**Goal:** Pace SMTP sends across replicas, preserve uncertain acceptance evidence, and expose fail-closed retry safety.
**Architecture:** Reuse PostgreSQL, the worker and durable outbox. A shared SMTP budget reserves capacity before sending; per-delivery attempts record only opaque IDs and sanitized numeric outcomes. Notification owns acceptance safety; Engagement consumes it before reopening failed recipients.
**Tech Stack:** Go, PostgreSQL, existing SMTP and Service Bus clients; no new dependencies.
**Spec:** Approved design in the current chat, 2026-10-18.

## Global constraints
- Additive migrations; legacy email history remains unknown, never backfilled as safe.
- Actual ACS hourly quota is 100. Default minute ceiling is conservatively 20 (not claimed to be the actual quota); reserve 5/minute and 20/hour for transactional messages. All are validated configuration.
- General errors retain five attempts; rate-limit waits do not consume that failure budget and remain bounded by original expiry. Do not sleep while holding a broker message for the quota window.
- Campaign acceptance_unknown stops for reconciliation. Account messages retain existing at-least-once behavior; uncertainty stays durable.
- No recipients, payloads, tokens or raw SMTP responses in attempts/logs. Attempt rows cascade under existing parent-message retention.
- Producer PR precedes consumer PR at any future authorized rollout; these PRs are not deployed by this task.

## Review focus
- Concurrent replicas must not overrun minute/hour windows or starve reserved transactional capacity.
- Final DATA 4xx/5xx are definitive rejections; missing final transport response is unknown.
- Crashes between send and persistence must never make a later retry appear proven safe.
- Legacy/history-missing, expired, purged, accepted and uncertain messages must fail closed.
- Existing consent, DSR delivery fences, expiry and Web Push behavior must remain intact.

### Task 1: SMTP response classification
- [x] Add failing SMTP fixture cases for final DATA 451/550 and explicit throttling, retaining lost-response and redaction cases.
- [x] Classify final protocol rejections separately; preserve sanitized SMTP/enhanced codes and stage.
- [x] Run provider tests.

### Task 2: Durable email budget and evidence
- [x] Add failing PostgreSQL tests for concurrent budgets, reserved capacity, cooldown, attempt outcomes, legacy safety and cascade retention.
- [x] Add migration 006 for attempts, provider budget state, complete-history marker and general failure count; wire validated config and worker behavior.
- [x] Preserve failed/unknown attempts and incomplete starts, expose safeToRetry on existing GET, document additive contract and operations.
- [x] Run unit/integration/race tests, vet, build, OpenAPI and release/governance checks.

### Task 3: Cross-service verification and PR
- [x] Verify consumer missing-field rejection and safe subset retries against the additive producer contract.
- [ ] Review whole branch, fix important findings with regression tests, run CI and open a PR without auto-merge.
