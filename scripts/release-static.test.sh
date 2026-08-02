#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="${repo_root}/.github/workflows/release.yml"
dockerfile="${repo_root}/Dockerfile"
bicep="${repo_root}/infra/main.bicep"
secret_bicep="${repo_root}/infra/secret-scope.bicep"
alerts_bicep="${repo_root}/infra/alerts.bicep"
production_release="${repo_root}/infra/production-release.env"
secret_preflight="${repo_root}/scripts/verify-secret-scope.sh"
readme="${repo_root}/infra/README.md"

fail() {
  echo "release static test failed: $1" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local pattern="$2"
  local description="$3"
  PATTERN="${pattern}" perl -0777 -e '
    $content = do { local $/; <> };
    exit($content =~ /$ENV{PATTERN}/m ? 0 : 1);
  ' "${file}" || fail "${description}"
}

assert_not_contains() {
  local file="$1"
  local pattern="$2"
  local description="$3"
  if PATTERN="${pattern}" perl -0777 -e '
    $content = do { local $/; <> };
    exit($content =~ /$ENV{PATTERN}/m ? 0 : 1);
  ' "${file}"; then
    fail "${description}"
  fi
}

[[ -f "${workflow}" ]] || fail "release workflow is missing"
bash "${repo_root}/scripts/verify-secret-scope.test.sh"

assert_contains "${workflow}" 'contents:[[:space:]]*read' "contents permission must be read-only"
assert_contains "${workflow}" '(?s)^permissions:\n  contents: read\n\nconcurrency:' "workflow permissions must only read contents"
assert_contains "${workflow}" '(?s)  plan:\n.*?    needs: verify\n.*?    permissions:\n      contents: read\n      id-token: write' "plan must depend on verify and receive OIDC"
assert_contains "${workflow}" '(?s)  deploy:\n.*?    needs: plan\n.*?    environment: production\n.*?    permissions:\n      contents: read\n      id-token: write' "deploy must depend on plan, require production approval, and receive fresh OIDC"
grep -Fq 'if: ${{ vars.PRODUCTION_DEPLOY_ENABLED == '\''true'\'' }}' "${workflow}" ||
  fail "deploy must remain disabled until the protected production gate is configured"
[[ "$(grep -Ec 'id-token:[[:space:]]*write' "${workflow}")" == "2" ]] || fail "plan and deploy must each receive OIDC"
assert_contains "${workflow}" 'group:[[:space:]]*notification-production' "production concurrency group is missing"
assert_contains "${workflow}" 'cancel-in-progress:[[:space:]]*false' "production releases must not cancel in progress"
assert_contains "${workflow}" 'POSTGRES_DB:[[:space:]]*notification_test' "Postgres test database is missing"
assert_contains "${workflow}" 'go test \./\.\.\.' "unit tests are missing"
assert_contains "${workflow}" 'go test -tags=integration \./\.\.\. -count=1' "integration tests are missing"
assert_contains "${workflow}" 'go vet \./\.\.\.' "go vet is missing"
assert_contains "${workflow}" 'bash scripts/release-static\.test\.sh' "release static test is not run by CI"
assert_contains "${workflow}" 'uses: actions/checkout@[0-9a-f]{40}[[:space:]]+# v7\.0\.1' "checkout must use a full SHA with the approved version comment"
assert_contains "${workflow}" 'uses: actions/setup-go@[0-9a-f]{40}[[:space:]]+# v7\.0\.0' "setup-go must use a full SHA with the approved version comment"
assert_contains "${workflow}" 'uses: azure/login@[0-9a-f]{40}[[:space:]]+# v3\.0\.0' "azure-login must use a full SHA with the approved version comment"
assert_not_contains "${workflow}" 'uses: (actions/checkout|actions/setup-go|azure/login)@v[0-9]' "release actions must not use mutable tags"
assert_contains "${workflow}" 'IMAGE_TAG=main-\$\{GITHUB_SHA::7\}' "immutable short-SHA tag is missing"
assert_not_contains "${workflow}" 'IMAGE_REPOSITORY}:latest|alive/notification-api:latest' "latest image tag must not be published"
assert_contains "${workflow}" 'az acr repository show' "ACR digest must be resolved with a supported Azure CLI command"
assert_contains "${workflow}" 'image_ref="\$\{ACR_LOGIN_SERVER\}/\$\{IMAGE_REPOSITORY\}@\$\{image_digest\}"' "digest image reference is missing"
assert_contains "${workflow}" '(?s)outputs:\n      image_ref: \$\{\{ steps\.image\.outputs\.image_ref \}\}' "plan must expose the digest image reference"
assert_contains "${workflow}" 'IMAGE_REF: \$\{\{ needs\.plan\.outputs\.image_ref \}\}' "deploy must consume the planned digest image reference"
assert_contains "${workflow}" 'imageDigest="\$\{IMAGE_DIGEST\}"' "Bicep deployments must receive the raw image digest"
assert_not_contains "${workflow}" 'migrationImage=|runtimeImage=' "legacy image parameters must not be passed"
assert_contains "${workflow}" 'actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a' "what-if artifact upload must use the approved immutable action"
[[ "$(grep -Ec 'az deployment group what-if' "${workflow}")" -ge "4" ]] ||
  fail "alerts and main what-if must run in both plan and deploy"
[[ "$(grep -Ec -- '--result-format FullResourcePayloads' "${workflow}")" -ge "4" ]] ||
  fail "all release what-if commands must save full resource payloads"
assert_not_contains "${workflow}" '--mode Complete' "resource-group what-if must remain incremental"
assert_contains "${workflow}" '(?s)  deploy:\n.*?Capture runtime state and preflight monitoring.*?az deployment group what-if.*?infra/alerts\.bicep.*?az deployment group what-if.*?infra/main\.bicep.*?az deployment group create' "deploy must finish preflight and both what-if checks before any deployment create"
assert_contains "${workflow}" 'vars\.SMTP_ADDR' "SMTP_ADDR must come from repository variables"
assert_contains "${workflow}" 'vars\.SMTP_FROM' "SMTP_FROM must come from repository variables"
assert_contains "${workflow}" 'vars\.SMTP_AUTHENTICATION_ENABLED' "SMTP auth flag must come from repository variables"
assert_contains "${workflow}" 'provisionPermissions=false' "CI must not provision IAM or Key Vault permissions"
[[ "$(grep -Fc 'bash scripts/verify-secret-scope.sh' "${workflow}")" == "2" ]] ||
  fail "plan and deploy must both verify the dedicated secret scope"
[[ "$(grep -Fc 'activeEncryptionKeyID="${ACTIVE_ENCRYPTION_KEY_ID}"' "${workflow}")" == "4" ]] ||
  fail "every runtime plan/apply must use the reviewable active encryption key ID"
[[ "$(grep -Fc 'activeHashKeyID="${ACTIVE_HASH_KEY_ID}"' "${workflow}")" == "4" ]] ||
  fail "every runtime plan/apply must use the reviewable active hash key ID"
[[ "$(grep -Fc 'notificationVaultName="${NOTIFICATION_VAULT_NAME}"' "${workflow}")" == "4" ]] ||
  fail "every runtime plan/apply must use the reviewed dedicated vault name"
assert_contains "${workflow}" 'deployRuntime=false' "migration-only deployment is missing"
assert_contains "${workflow}" 'az containerapp job start' "migration job is not started"
assert_contains "${workflow}" 'Succeeded' "migration success is not enforced"
assert_contains "${workflow}" 'deployRuntime=true' "runtime deployment is missing"
assert_contains "${workflow}" 'notification-api' "API readiness check is missing"
assert_contains "${workflow}" 'notification-worker' "worker readiness check is missing"
assert_contains "${workflow}" 'latestRevisionName' "latest revision readiness is not checked"
assert_contains "${workflow}" 'latestReadyRevisionName' "latest ready revision is not checked"
assert_contains "${workflow}" 'az containerapp revision show' "ready revision image is not inspected"
assert_contains "${workflow}" 'properties\.template\.containers\[0\]\.image' "ready revision image is not checked"
assert_contains "${workflow}" 'properties\.latestReadyRevisionName' "rollback must capture Ready revisions"
assert_contains "${workflow}" 'az containerapp revision copy' "rollback must copy the previous Ready revision"
assert_contains "${workflow}" 'rollback_status=0' "API and worker rollback outcomes must be aggregated"
assert_contains "${workflow}" 'script -q' "Container Apps exec TTY wrapper is missing"
assert_contains "${workflow}" 'timeout 60s script -q' "Container Apps exec must have a hard timeout"
assert_contains "${workflow}" 'timeout-minutes:[[:space:]]*[0-9]+' "deploy job must have a bounded timeout"
assert_contains "${workflow}" '/usr/bin/wget' "gateway readiness probe must use the existing wget"
assert_contains "${workflow}" "grep -Eq 'HTTP/1" "Dapr probe must validate HTTP 200"
assert_contains "${workflow}" "grep -Eq '\"status\"" "Dapr probe must validate the response body"

assert_contains "${dockerfile}" '^FROM golang:1\.25-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS builder$' "Go base image must use the approved multi-arch digest"
assert_contains "${dockerfile}" '^FROM gcr\.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35$' "runtime base image must use the approved multi-arch digest"

assert_contains "${bicep}" 'param provisionPermissions bool = true' "provisionPermissions must default to true"
assert_contains "${bicep}" 'zoneRedundant:[[:space:]]*true' "Service Bus zone redundancy must match production"
assert_contains "${bicep}" "param notificationVaultName string = 'alive-notify-[^']+'" "notification must use a dedicated vault"
assert_contains "${bicep}" "(?s)resource vault 'Microsoft\\.KeyVault/vaults@[^']+' existing = \\{.*?name: notificationVaultName" "runtime secrets must reference the dedicated vault"
assert_contains "${bicep}" 'NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID' "active encryption key ID is missing"
assert_contains "${bicep}" 'NOTIFICATION_ENCRYPTION_KEYS_JSON' "encryption keyring is missing"
assert_contains "${bicep}" 'NOTIFICATION_ACTIVE_HASH_KEY_ID' "active hash key ID is missing"
assert_contains "${bicep}" 'NOTIFICATION_HASH_KEYS_JSON' "hash keyring is missing"
[[ "$(grep -Fc '@minLength(71)' "${bicep}")" == "1" &&
   "$(grep -Fc '@maxLength(71)' "${bicep}")" == "1" &&
   "$(grep -Fc 'param imageDigest string' "${bicep}")" == "1" ]] ||
  fail "imageDigest must have the exact sha256 digest length"
grep -Fq "alive/notification-api@\${imageDigest}" "${bicep}" ||
  fail "Bicep must construct the ACR digest reference internally"
assert_not_contains "${bicep}" 'param (migrationImage|runtimeImage) string' "legacy image parameters must be removed"
assert_contains "${bicep}" "param legacyVaultName string = 'alive-vault'" "cutover must preserve the shared-vault rollback aliases"
assert_contains "${bicep}" "name: 'database-url-v2'" "new revisions must use a distinct dedicated-vault alias"
assert_contains "${bicep}" "name: 'database-url'.*?legacyDatabaseSecretUrl" "old revisions must retain their shared-vault alias"
assert_not_contains "${bicep}" "secrets: \\['get', 'list'\\]" "runtime template must not grant shared-vault permissions"
assert_contains "${secret_bicep}" 'enableRbacAuthorization:[[:space:]]*true' "notification vault must use RBAC"
assert_contains "${secret_bicep}" 'enablePurgeProtection:[[:space:]]*true' "notification vault must enable purge protection"
assert_contains "${secret_bicep}" "defaultAction:[[:space:]]*'Deny'" "notification vault network must default deny"
assert_contains "${secret_bicep}" 'scope:[[:space:]]*(databaseSecret|encryptionSecret|hashSecret|encryptionKeysSecret|hashKeysSecret|smtpUsernameSecret|smtpPasswordSecret)' "secret permissions must use secret-level scopes"
assert_not_contains "${secret_bicep}" "secrets:[[:space:]]*\\[[^]]*list" "notification identities must not list vault secrets"
for alert in \
  notification-api-rate-limited \
  notification-api-5xx \
  notification-api-restarts \
  notification-worker-restarts \
  notification-sb-deadlettered \
  notification-sb-backlog-stuck \
  notification-sb-server-errors \
  notification-sb-throttled \
  notification-acceptance-unknown \
  notification-outbox-delayed \
  notification-provider-failure-ratio \
  notification-provider-config-failure \
  notification-worker-scaler-failure
do
  assert_contains "${alerts_bicep}" "name: '${alert}'" "${alert} must remain repo-managed"
done
[[ "$(grep -Fc 'autoMitigate: true' "${alerts_bicep}")" == "13" ]] ||
  fail "all notification alerts must preserve auto mitigation"
assert_contains "${alerts_bicep}" 'smtp delivery failed' "provider alerts must match the pre-cutover log format"
assert_contains "${alerts_bicep}" 'smtp delivery accepted' "provider ratio alert must match the pre-cutover success format"
assert_contains "${production_release}" '^NOTIFICATION_VAULT_NAME=alive-notify-[a-z0-9]+$' "production vault name must be reviewable"
notification_vault_name="$(sed -n 's/^NOTIFICATION_VAULT_NAME=//p' "${production_release}")"
[[ "${#notification_vault_name}" -ge 3 && "${#notification_vault_name}" -le 24 ]] ||
  fail "production vault name must satisfy Azure's 3-24 character limit"
assert_contains "${production_release}" '^ACTIVE_ENCRYPTION_KEY_ID=[A-Za-z0-9._-]+$' "active encryption key ID must be reviewable"
assert_contains "${production_release}" '^ACTIVE_HASH_KEY_ID=[A-Za-z0-9._-]+$' "active hash key ID must be reviewable"
assert_contains "${secret_preflight}" 'az resource show --ids' "secret preflight must use ARM metadata without reading values"
assert_not_contains "${secret_preflight}" 'az keyvault secret show' "secret preflight must not access secret values"
assert_contains "${secret_preflight}" 'role assignment list --assignee-object-id "\$principal" --all' "preflight must inspect all principal assignments"
assert_not_contains "${secret_preflight}" 'role assignment list[^\\n]*--scope[^\\n]*--all' "role queries must not combine --scope and --all"
assert_contains "${secret_preflight}" 'enableRbacAuthorization == true' "secret preflight must enforce vault RBAC"
assert_contains "${secret_preflight}" 'enablePurgeProtection == true' "secret preflight must enforce purge protection"
assert_contains "${secret_preflight}" 'defaultAction == "Deny"' "secret preflight must enforce default-deny networking"
grep -Fq '(.properties.networkAcls.virtualNetworkRules | length) == 1' "${secret_preflight}" ||
  fail "secret preflight must enforce the exact ACA subnet rule"
[[ "$(grep -Fc "{ name: 'DB_MAX_OPEN_CONNS', value: '2' }" "${bicep}")" == "1" ]] ||
  fail "API and worker must share the two-connection runtime limit"
[[ "$(grep -Fc "{ name: 'DB_MAX_OPEN_CONNS', value: '1' }" "${bicep}")" == "1" ]] ||
  fail "migration must use one database connection"
[[ "$(grep -Fc "{ name: 'DB_MAX_IDLE_CONNS', value: '1' }" "${bicep}")" == "2" ]] ||
  fail "runtime and migration idle connection limits are missing"
[[ "$(grep -Fc "{ name: 'DB_CONN_MAX_LIFETIME', value: '30m' }" "${bicep}")" == "2" ]] ||
  fail "runtime and migration connection lifetimes are missing"
assert_not_contains "${readme}" 'optional `latest`|`:latest`' "README must not describe a mutable latest image"
for resource in \
  apiAcrPull \
  workerAcrPull \
  migrateAcrPull \
  apiServiceBusSender \
  workerServiceBusReceiver
do
  assert_contains \
    "${bicep}" \
    "resource ${resource} '[^']+' = if \\(provisionPermissions\\)" \
    "${resource} must be conditional"
done

echo "release static test ok"
