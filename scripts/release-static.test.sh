#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="${repo_root}/.github/workflows/release.yml"
bicep="${repo_root}/infra/main.bicep"

fail() {
  echo "release static test failed: $1" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local pattern="$2"
  local description="$3"
  rg --quiet --multiline "${pattern}" "${file}" || fail "${description}"
}

[[ -f "${workflow}" ]] || fail "release workflow is missing"

assert_contains "${workflow}" 'contents:[[:space:]]*read' "contents permission must be read-only"
assert_contains "${workflow}" 'id-token:[[:space:]]*write' "OIDC permission is missing"
assert_contains "${workflow}" 'group:[[:space:]]*notification-production' "production concurrency group is missing"
assert_contains "${workflow}" 'cancel-in-progress:[[:space:]]*false' "production releases must not cancel in progress"
assert_contains "${workflow}" 'POSTGRES_DB:[[:space:]]*notification_test' "Postgres test database is missing"
assert_contains "${workflow}" 'go test \./\.\.\.' "unit tests are missing"
assert_contains "${workflow}" 'go test -tags=integration \./\.\.\. -count=1' "integration tests are missing"
assert_contains "${workflow}" 'go vet \./\.\.\.' "go vet is missing"
assert_contains "${workflow}" 'IMAGE_TAG=main-\$\{GITHUB_SHA::7\}' "immutable short-SHA tag is missing"
assert_contains "${workflow}" 'vars\.SMTP_ADDR' "SMTP_ADDR must come from repository variables"
assert_contains "${workflow}" 'vars\.SMTP_FROM' "SMTP_FROM must come from repository variables"
assert_contains "${workflow}" 'vars\.SMTP_AUTHENTICATION_ENABLED' "SMTP auth flag must come from repository variables"
assert_contains "${workflow}" 'provisionPermissions=false' "CI must not provision IAM or Key Vault permissions"
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
assert_contains "${workflow}" 'script -q' "Container Apps exec TTY wrapper is missing"
assert_contains "${workflow}" '/usr/bin/wget' "gateway readiness probe must use the existing wget"
assert_contains "${workflow}" "grep -Eq 'HTTP/1" "Dapr probe must validate HTTP 200"
assert_contains "${workflow}" "grep -Eq '\"status\"" "Dapr probe must validate the response body"

assert_contains "${bicep}" 'param provisionPermissions bool = true' "provisionPermissions must default to true"
for resource in \
  apiAcrPull \
  workerAcrPull \
  migrateAcrPull \
  apiServiceBusSender \
  workerServiceBusReceiver \
  notificationSecretAccess
do
  assert_contains \
    "${bicep}" \
    "resource ${resource} '[^']+' = if \\(provisionPermissions\\)" \
    "${resource} must be conditional"
done

echo "release static test ok"
