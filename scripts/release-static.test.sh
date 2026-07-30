#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="${repo_root}/.github/workflows/release.yml"
bicep="${repo_root}/infra/main.bicep"
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

assert_contains "${workflow}" 'contents:[[:space:]]*read' "contents permission must be read-only"
assert_contains "${workflow}" '(?s)^permissions:\n  contents: read\n\nconcurrency:' "workflow permissions must only read contents"
assert_contains "${workflow}" '(?s)  deploy:\n.*?    needs: verify\n.*?    permissions:\n      contents: read\n      id-token: write' "deploy must depend on verify and alone receive OIDC"
[[ "$(grep -Ec 'id-token:[[:space:]]*write' "${workflow}")" == "1" ]] || fail "OIDC permission must appear exactly once"
assert_contains "${workflow}" 'group:[[:space:]]*notification-production' "production concurrency group is missing"
assert_contains "${workflow}" 'cancel-in-progress:[[:space:]]*false' "production releases must not cancel in progress"
assert_contains "${workflow}" 'POSTGRES_DB:[[:space:]]*notification_test' "Postgres test database is missing"
assert_contains "${workflow}" 'go test \./\.\.\.' "unit tests are missing"
assert_contains "${workflow}" 'go test -tags=integration \./\.\.\. -count=1' "integration tests are missing"
assert_contains "${workflow}" 'go vet \./\.\.\.' "go vet is missing"
assert_contains "${workflow}" 'bash scripts/release-static\.test\.sh' "release static test is not run by CI"
assert_contains "${workflow}" 'uses: actions/checkout@[0-9a-f]{40}[[:space:]]+# v4' "checkout must use a full SHA with a v4 comment"
assert_contains "${workflow}" 'uses: actions/setup-go@[0-9a-f]{40}[[:space:]]+# v5' "setup-go must use a full SHA with a v5 comment"
assert_contains "${workflow}" 'uses: azure/login@[0-9a-f]{40}[[:space:]]+# v2' "azure-login must use a full SHA with a v2 comment"
assert_not_contains "${workflow}" 'uses: (actions/checkout|actions/setup-go|azure/login)@v[0-9]' "release actions must not use mutable tags"
assert_contains "${workflow}" 'IMAGE_TAG=main-\$\{GITHUB_SHA::7\}' "immutable short-SHA tag is missing"
assert_not_contains "${workflow}" 'IMAGE_REPOSITORY}:latest|alive/notification-api:latest' "latest image tag must not be published"
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
assert_contains "${workflow}" 'properties\.latestReadyRevisionName' "rollback must capture Ready revisions"
assert_contains "${workflow}" 'az containerapp revision copy' "rollback must copy the previous Ready revision"
assert_contains "${workflow}" 'rollback_status=0' "API and worker rollback outcomes must be aggregated"
assert_contains "${workflow}" 'script -q' "Container Apps exec TTY wrapper is missing"
assert_contains "${workflow}" 'timeout 60s script -q' "Container Apps exec must have a hard timeout"
assert_contains "${workflow}" 'timeout-minutes:[[:space:]]*[0-9]+' "deploy job must have a bounded timeout"
assert_contains "${workflow}" '/usr/bin/wget' "gateway readiness probe must use the existing wget"
assert_contains "${workflow}" "grep -Eq 'HTTP/1" "Dapr probe must validate HTTP 200"
assert_contains "${workflow}" "grep -Eq '\"status\"" "Dapr probe must validate the response body"

assert_contains "${bicep}" 'param provisionPermissions bool = true' "provisionPermissions must default to true"
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
  workerServiceBusReceiver \
  notificationSecretAccess
do
  assert_contains \
    "${bicep}" \
    "resource ${resource} '[^']+' = if \\(provisionPermissions\\)" \
    "${resource} must be conditional"
done

echo "release static test ok"
