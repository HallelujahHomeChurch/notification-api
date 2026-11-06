# Notification Azure infrastructure

This directory manages the notification Service Bus queue, dedicated secret
scope, API, worker, migration job, identities, and alerts in resource group
`alive`.

## Prerequisites

- Existing `alive-env`, `alive` ACR, `alive-vnet/aca`, `alive-env-logs`, and
  `RecommendedAlertRules-AG-1`.
- Private PostgreSQL reachable from the Container Apps environment.
- SMTP endpoint and sender, plus one generated VAPID key pair.
- GitHub environment `production` with a required reviewer, self-review
  disabled, and deployment branches limited to `main`.
- Repository variable `PRODUCTION_DEPLOY_ENABLED=true` only after that
  environment protection is verified. Without it the deploy job is skipped.

The runtime template accepts one 71-character `sha256:` digest and constructs
the ACR image reference itself. Tags and `latest` are not deployment inputs.

## Bootstrap the dedicated vault

`secret-scope.bicep` creates an RBAC-enabled, purge-protected vault restricted
to the ACA subnet. Its secure parameters create eight notification-only
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
    smtpPassword="${SMTP_PASSWORD}" \
    vapidPrivateKey="${VAPID_PRIVATE_KEY}"
```

After review, an administrator may replace `what-if` with `create`. Never use
shell tracing, echo these variables, or save them in a parameter file. Wait for
RBAC propagation, then run `scripts/verify-secret-scope.sh`; it validates the
eight ARM secret resources and exact secret-level assignments without reading
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
6. Dedicated vault, eight secrets, and exact RBAC preflight.
7. A fresh what-if immediately before apply.
8. Alert deployment, migration-only deployment, and one successful migration.
9. API/worker deployment by digest, exact revision verification, and gateway
   Dapr readiness smoke.
10. Automatic rollback to the previous ready revisions after failed runtime
    verification. Transitional shared-vault aliases keep those revisions valid.

Routine CI passes `provisionPermissions=false`. Initial ACR and Service Bus role
bootstrap remains an explicit administrator operation.

## Engagement status-only release

After Account and Engagement both use the pinned Notification HTTPS bindings,
dispatch the existing production workflow with `retire_dapr_revisions` set to
the exact serving Notification API, Account and Engagement revision names,
separated by spaces. This changes only the API's sidecar configuration using
the existing image. It verifies the same three HTTPS bindings and listener
before and after, checks older consumer replicas are drained and preserves
the Worker. Routine releases preserve the observed API Dapr state.

The regular production release preserves the deployed private status binding.
Use the same owner workflow on main for a status-only transition, with
`status_transition`, `status_expected_revision`, `status_source_release_run`,
and, for enable, `status_version`. Run with `status_reviewed_plan` empty to
obtain the Provider what-if fingerprint. Review the API-only delta before
dispatching the same inputs with that exact fingerprint. Restore every active
Engagement revision to Dapr before disabling the Notification ingress.

Bootstrap the dedicated secret and its two exact API identity reader
assignments with the reviewed `engagement-status-secret-access.bicep` template
using the existing administrator procedure. Use a 32-byte random credential
encoded as hex and submit the secure parameter through standard input. Supply
the pinned version ID to both owners after verifying role scope.

## Private HTTPS writes

The current owner release also admits Account enqueue and cleanup and
Engagement enqueue on the same private HTTPS listener when their individual
credentials are configured. The existing Dapr listener keeps serving callers
through the transition. Each credential grants access only to its documented
method and paths.

Before the one normal production release, use the existing administrator
procedure to preview and apply `private-write-secret-access.bicep`. Generate
two independent 32-byte random credentials encoded as hex. Submit the secure
parameters through standard input and verify the four secret-level reader
assignments. Set the reviewed version IDs in
`NOTIFICATION_ACCOUNT_PRIVATE_SECRET_VERSION` and
`NOTIFICATION_ENGAGEMENT_SEND_SECRET_VERSION` for Notification and the
corresponding owner CD. Deploy Notification first and verify its pinned
bindings and serving revision before deploying either caller.

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

## SES feedback listener with SMTP sending

The optional, typed `sesFeedback` release input configures only the existing
worker's SNS/SQS listener. Its absence leaves the listener off. Set the
non-secret repository variable `SES_FEEDBACK_CONFIG` to the reviewed JSON object
with `region`, `roleArn`, `tokenScope`, `from`, `configurationSet`, `queueUrl`,
`dlqUrl` and `topicArn`. Plan and deploy pass the same input through all what-if
and deployment operations. The worker still receives `EMAIL_PROVIDER=smtp`;
this input cannot enable SES sending. The API continues to use the SMTP default.

The parent Azure platform Terraform root declares the same worker environment
fields; it is not the runtime deployment path. Merge and apply the dedicated
email-delivery infrastructure root first, read back the queue/topic/role and
paused configuration set, then run the normal merged-main Notification release.
Review the complete ARM what-if before deployment; preserve existing SMTP and
Key Vault settings. Do not use a manual Container Apps environment patch.
