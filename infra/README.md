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

`alive-vault` currently uses access policies, so the template adds secret-read
policies for the three workload identities. Do not switch the shared vault to
RBAC as part of this deployment.

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

Deploy infrastructure and update only the migration job first:

```bash
az deployment group create \
  --resource-group alive \
  --template-file infra/main.bicep \
  --parameters infra/main.bicepparam \
  --parameters deployRuntime=false

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
  --parameters deployRuntime=true
```

Incremental deployment leaves existing API and worker revisions unchanged when
`deployRuntime=false`. A failed migration therefore cannot roll out new runtime
images.

The API publishes with `notification-api-identity`; the worker receives and
scales with `notification-worker-identity`. No Service Bus connection string is
stored.
