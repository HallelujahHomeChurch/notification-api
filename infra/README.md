# Notification Azure infrastructure

This template provisions the notification Service Bus queue, API, worker,
migration job, workload identities, and least-privilege role assignments in
the existing `alive` resource group.

## Prerequisites

- Existing `alive-env`, `alive` ACR, and `alive-vault`.
- Private PostgreSQL reachable at `172.16.68.4:5432`.
- SMTP endpoint and sender. When SMTP authentication is enabled, create
  `notification-smtp-username` and `notification-smtp-password` in
  `alive-vault` before deployment.
- A built image with an immutable `main-<short-sha>` tag. Both image parameters
  are required; there is no mutable `latest` default.

`alive-vault` currently uses access policies. An initial administrator
deployment must use `provisionPermissions=true` (the default) to create ACR,
Service Bus, and Key Vault access for the three workload identities. The
deploying principal must be allowed to create role assignments in addition to
updating the existing Key Vault. Do not switch the shared vault to RBAC as part
of this deployment.

## Bootstrap and validate

```bash
bash scripts/bootstrap-database.test.sh
bash scripts/bootstrap-database.sh

cp infra/main.example.bicepparam infra/main.bicepparam
# Set the immutable image and real SMTP values in the ignored parameter file.

az bicep build --file infra/main.bicep
az deployment group what-if \
  --resource-group alive \
  --template-file infra/main.bicep \
  --parameters infra/main.bicepparam
```

Use the default `provisionPermissions=true` for the initial administrator
bootstrap. After those permissions exist, routine deployments must pass
`provisionPermissions=false`; this allows the GitHub OIDC principal to remain a
Contributor without IAM write access.

Deploy infrastructure and update only the migration job first:

```bash
az deployment group create \
  --resource-group alive \
  --template-file infra/main.bicep \
  --parameters infra/main.bicepparam \
  --parameters deployRuntime=false provisionPermissions=false

az containerapp job start \
  --resource-group alive \
  --name notification-migrate
```

Only after the migration execution reports `Succeeded`, deploy runtime
revisions:

```bash
az deployment group create \
  --resource-group alive \
  --template-file infra/main.bicep \
  --parameters infra/main.bicepparam \
  --parameters deployRuntime=true provisionPermissions=false
```

Incremental deployment leaves existing API and worker revisions unchanged when
`deployRuntime=false`. A failed migration therefore cannot roll out new runtime
images.

The API publishes with `notification-api-identity`; the worker receives and
scales with `notification-worker-identity`. No Service Bus connection string is
stored.

## GitHub Actions release

`.github/workflows/release.yml` runs for relevant changes pushed to `main` and
can also be started manually. Configure the immutable repository OIDC binding
outside this repository, then add these repository variables:

- `AZURE_CLIENT_ID`
- `AZURE_TENANT_ID`
- `AZURE_SUBSCRIPTION_ID`
- `SMTP_ADDR`
- `SMTP_FROM`
- `SMTP_AUTHENTICATION_ENABLED` (`true` or `false`)

The workflow fails before build or deployment when any variable is missing or
the SMTP authentication flag is invalid. SMTP credentials are not GitHub
variables or secrets; when authentication is enabled, only
`notification-smtp-username` and `notification-smtp-password` in `alive-vault`
are used.

Each release tests the service against PostgreSQL `notification_test`, builds
`main-<short-sha>` and optional `latest` ACR tags, and deploys in this order:

1. Update only `notification-migrate` with `deployRuntime=false`.
2. Start one migration execution and require that exact execution to report
   `Succeeded`.
3. Update the API and worker with `deployRuntime=true`.
4. Require both latest revisions to be ready with the exact immutable image.
5. Invoke the notification API `/ready` endpoint through Dapr from
   `api-gateway` and validate both HTTP 200 and the readiness response body.

CI always passes `provisionPermissions=false`; permission bootstrap remains an
explicit administrator operation.
