# Repository Instructions

## Delivery Policy

- Treat `main` as the production branch. Do not commit or push directly to `main`.
- Create a focused branch for every task and submit changes through a pull request.
- CI must pass before merge. Do not bypass, ignore, or merge around a failing required check.
- Keep changes small and reviewable, and use squash merge unless the repository explicitly requires another strategy.
- For deployable applications and services, a merge to `main` is the release boundary: build an immutable artifact, deploy it through CI/CD, then verify health and the production route.
- Never deploy an unmerged local commit or replace the CI/CD path with an ad hoc production command.
- If deployment fails, preserve the last healthy revision, report the failure, and use the repository rollback path.

## Architecture and local verification

- Private durable provider-delivery service for Account and Engagement. Owns idempotent notification intents, outbox/queue/retention and email/Web Push delivery; it does not own campaign audiences or account credentials.
- `cmd/notification` selects migrate/api/worker; API and worker are separate processes. `internal/{httpapi,service,store,outbox}` admit/persist intents; `internal/{queue,worker,providers,templates}` consume and deliver. `cmd/diagnose-notification` is a separate diagnostic tool, not normal startup.
- Read [README.md](README.md), [docs/openapi.yaml](docs/openapi.yaml), [docs/runbook.md](docs/runbook.md), [go.mod](go.mod), `internal/config` and [.github/workflows/ci.yml](.github/workflows/ci.yml). Use exported task-local configuration and `go run ./cmd/notification migrate`, then api/worker. PostgreSQL and Service Bus are mandatory; the local emulator uses SERVICEBUS_CONNECTION_STRING. Worker additionally needs SMTP/VAPID; there is no log-only delivery fallback.
- Run `go test ./...`, `go test -tags=integration ./... -count=1` and `go vet ./...` with the CI's disposable TEST_DATABASE_URL and other test dependencies. CI also validates contracts/security/image. Provider/emulator readiness and real send acceptance are separate verification states.
- Preserve exact callers, idempotency, stable Message-ID, quota and ambiguous-acceptance handling. Never infer exactly-once SMTP, replay historical failures or enable production cutover from a local test.

The commands above are source-defined entrypoints, not a record of successful execution. Report installation, local tests, Linux startup, CI and live acceptance separately.

## Multi-repository Codex tasks

- First read the checked-out `hhc-planning/AGENTS.md` and
  `hhc-planning/docs/cloud-environments.md` at the planning revision recorded in
  the task. Sibling repositories do not inherit those instructions automatically.
- This repository owns its setup and verification: read its README, runtime
  manifests and CI before choosing commands. Report missing dependencies or
  unverified Linux startup instead of claiming a successful run.
- Preserve unrelated edits and active writer ownership. Use an isolated task
  branch and follow the task's explicit review, merge and deployment authorization.
- Pure human documentation uses lightweight validation. Source, workflow, runtime
  configuration, executable skills and mixed changes retain full verification.
  Record read-only inspection, test execution, CI and live acceptance separately.

If no sibling planning checkout exists, use the [single-repository fallback](https://github.com/HallelujahHomeChurch/hhc-planning/blob/73ee1af2a1ff828d72421a73626f45ff4ce04a6f/docs/cloud-environments.md#single-repository-checkout)
to read the same entry documents through authorized GitHub access at the task’s
immutable planning revision. Record that revision; report the exact inaccessible
target/error without expanding permissions or copying private text here.
