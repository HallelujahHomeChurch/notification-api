# Notification Azure infrastructure

This directory manages the notification Service Bus queue, dedicated secret
scope, API, worker, migration job, identities, and alerts in resource group
`alive`.

## Prerequisites

- Existing `alive-env`, `alive` ACR, `alive-vnet/aca`, `alive-env-logs`, and
  `RecommendedAlertRules-AG-1`.
- Private PostgreSQL reachable from the Container Apps environment.
- SMTP endpoint and sender.
- GitHub environment `production` with a required reviewer, self-review
  disabled, and deployment branches limited to `main`.
- Repository variable `PRODUCTION_DEPLOY_ENABLED=true` only after that
  environment protection is verified. Without it the deploy job is skipped.

The runtime template accepts one 71-character `sha256:` digest and constructs
the ACR image reference itself. Tags and `latest` are not deployment inputs.

## Bootstrap the dedicated vault

`secret-scope.bicep` creates an RBAC-enabled, purge-protected vault restricted
to the ACA subnet. Its secure parameters create seven notification-only
secrets. Role assignments are scoped to the exact secrets consumed by each
identity; no notification identity receives vault-wide `list`.

Before the first cutover, read the existing values into shell variables without
printing them, build both keyring JSON values with `legacy-v1`, and run:

```bash
az deployment group what-if \
  --resource-group alive \
  --template-file infra/secret-scope.bicep \
  --parameters \
    databaseURL="${DATABASE_URL}" \
    dataEncryptionKey="${DATA_ENCRYPTION_KEY}" \
    hashKey="${HASH_KEY}" \
    encryptionKeysJSON="${ENCRYPTION_KEYS_JSON}" \
    hashKeysJSON="${HASH_KEYS_JSON}" \
    smtpUsername="${SMTP_USERNAME}" \
    smtpPassword="${SMTP_PASSWORD}"
```

After review, an administrator may replace `what-if` with `create`. Never use
shell tracing, echo these variables, or save them in a parameter file. Wait for
RBAC propagation, then run `scripts/verify-secret-scope.sh`; it validates the
seven ARM secret resources and exact secret-level assignments without reading
their values.

The initial runtime release supplies both legacy and versioned keyring
variables with active ID `legacy-v1`. This preserves rolling compatibility.
The active IDs and dedicated vault name live in the reviewed
`infra/production-release.env`. Activate a new key only in a separate approved
commit after both keyring secrets contain the new and retained old values.

During cutover, Container Apps retain the old shared-vault aliases and add
distinct `*-v2` aliases for the dedicated vault. New revisions use only `*-v2`;
old revisions remain rollback-capable. Remove the old aliases and the three
notification identity policies from shared `alive-vault` only in a later
approved release after the rollback drill and retention window. Do not alter
the shared vault authorization mode.

## Validate and deploy

```bash
bash scripts/bootstrap-database.test.sh
bash scripts/release-static.test.sh
az bicep build --file infra/secret-scope.bicep
az bicep build --file infra/main.bicep
az bicep build --file infra/alerts.bicep
```

The release workflow performs:

1. Unit, integration, vet, static release, and Bicep checks.
2. ACR build using `main-<short-sha>` only as a discovery label.
3. Digest resolution and complete `alerts.bicep` plus `main.bicep` what-if.
4. Upload of the what-if artifact.
5. GitHub `production` environment approval.
6. Dedicated vault, seven secrets, and exact RBAC preflight.
7. A fresh what-if immediately before apply.
8. Alert deployment, migration-only deployment, and one successful migration.
9. API/worker deployment by digest, exact revision verification, and gateway
   Dapr readiness smoke.
10. Automatic rollback to the previous ready revisions after failed runtime
    verification. Transitional shared-vault aliases keep those revisions valid.

Routine CI passes `provisionPermissions=false`. Initial ACR and Service Bus role
bootstrap remains an explicit administrator operation.

## Capacity and readiness

API and worker replicas each have capacity for two PostgreSQL connections.
Three API plus five worker replicas therefore cap runtime capacity at 16;
migration adds one. This is a ceiling, not a reservation. Before approval,
verify the shared 50-connection server has room for migration and brief
old/new revision overlap.

`/health` is process liveness. `/ready` verifies PostgreSQL. Queue and provider
clients are constructed before the HTTP server starts; readiness never sends
email or requests Service Bus management permission.

## Monitoring

`alerts.bicep` owns the current production notification alerts plus:

- Service Bus server errors and throttling;
- sustained queue backlog and DLQ presence;
- API 5xx, rate limiting, and API/worker restarts;
- delayed transactional outbox rows;
- SMTP acceptance-unknown, configuration failures, and sustained failure ratio;
- sustained KEDA scaler checks.

Every alert identifies the HHC platform owner, threshold/window, action group,
and runbook. Worker scale-to-zero is expected; backlog age, not replica count,
is the worker-absence signal. Provider queries match both the pre-cutover SMTP
log format and the structured event format during rolling deployment.
