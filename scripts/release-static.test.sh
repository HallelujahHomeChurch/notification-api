#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="${repo_root}/.github/workflows/release.yml"
ci_workflow="${repo_root}/.github/workflows/ci.yml"
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
[[ "$(grep -Ec 'id-token:[[:space:]]*write' "${workflow}")" == "3" ]] || fail "plan, deploy, and docs publication must each receive OIDC"
assert_contains "${workflow}" 'group:[[:space:]]*notification-production' "production concurrency group is missing"
assert_contains "${workflow}" 'cancel-in-progress:[[:space:]]*false' "production releases must not cancel in progress"
assert_contains "${workflow}" 'POSTGRES_DB:[[:space:]]*notification_test' "Postgres test database is missing"
assert_contains "${workflow}" 'go test \./\.\.\.' "unit tests are missing"
assert_contains "${workflow}" 'go test -tags=integration \./\.\.\. -count=1' "integration tests are missing"
assert_contains "${workflow}" 'go vet \./\.\.\.' "go vet is missing"
assert_contains "${workflow}" 'bash scripts/release-static\.test\.sh' "release static test is not run by CI"
scanner='ghcr.io/aquasecurity/trivy@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969'
[[ "$(grep -Fc "${scanner}" "${ci_workflow}")" == "2" ]] || fail "CI must pin both Trivy scans"
[[ "$(grep -Fc "${scanner}" "${workflow}")" == "6" ]] || fail "release must pin dependency, verification-image, and immutable-image scans"
grep -Fq 'fs --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1' "${ci_workflow}" || fail "CI dependency scan is missing"
grep -Fq 'image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 notification-api:verify' "${ci_workflow}" || fail "CI image scan is missing"
grep -Fq 'fs --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1' "${workflow}" || fail "release dependency scan is missing"
grep -Fq 'image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 notification-api:verify' "${workflow}" || fail "release verification-image scan is missing"
grep -Fq 'docker pull "${IMAGE_REF}"' "${workflow}" || fail "immutable release image pull is missing"
grep -Fq 'image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 "${IMAGE_REF}"' "${workflow}" || fail "immutable release image scan is missing"
scan_line="$(grep -nF 'name: Scan immutable image' "${workflow}" | cut -d: -f1)"
what_if_line="$(grep -nF 'name: Produce complete what-if plan' "${workflow}" | cut -d: -f1)"
[[ "${scan_line}" -lt "${what_if_line}" ]] || fail "immutable image scan must finish before release planning"
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
assert_contains "${workflow}" 'vars\.VAPID_PUBLIC_KEY' "VAPID_PUBLIC_KEY must come from repository variables"
assert_contains "${workflow}" 'vars\.VAPID_SUBJECT' "VAPID_SUBJECT must come from repository variables"
assert_contains "${bicep}" "param smtpFromName string = '哈利路亞家教會'" "SMTP sender display name must remain branded"
assert_contains "${bicep}" "NOTIFICATION_ALLOWED_CALLERS', value: 'account-api,engagement-api'" "notification callers must match contract"
assert_not_contains "${bicep}" "NOTIFICATION_ALLOWED_CALLERS', value: 'account-api,hhc-web-api,engagement-api'" "hhc-web-api must not call notification directly"
assert_contains "${bicep}" "name: 'SMTP_FROM_NAME', value: smtpFromName" "worker must receive the branded SMTP sender name"
assert_contains "${bicep}" 'minReplicas: 1' "notification worker must avoid scale-to-zero delivery latency"
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

assert_contains "${dockerfile}" '^FROM golang@sha256:1e0126852075c9c60731c8ba49088448b91f63e2aed97ca9d1a9791622a05946 AS builder$' "Go base image must use the approved multi-arch digest"
assert_contains "${dockerfile}" '^FROM gcr\.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35$' "runtime base image must use the approved multi-arch digest"

assert_contains "${bicep}" 'param provisionPermissions bool = true' "provisionPermissions must default to true"
assert_contains "${bicep}" 'zoneRedundant:[[:space:]]*true' "Service Bus zone redundancy must match production"
assert_contains "${bicep}" "param notificationVaultName string = 'alive-notify-[^']+'" "notification must use a dedicated vault"
assert_contains "${bicep}" "(?s)resource vault 'Microsoft\\.KeyVault/vaults@[^']+' existing = \\{.*?name: notificationVaultName" "runtime secrets must reference the dedicated vault"
assert_contains "${bicep}" 'NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID' "active encryption key ID is missing"
assert_contains "${bicep}" 'NOTIFICATION_ENCRYPTION_KEYS_JSON' "encryption keyring is missing"
assert_contains "${bicep}" 'NOTIFICATION_ACTIVE_HASH_KEY_ID' "active hash key ID is missing"
assert_contains "${bicep}" 'NOTIFICATION_HASH_KEYS_JSON' "hash keyring is missing"
[[ "$(grep -Fc "cpu: json('0.25')" "${bicep}")" == "3" &&
   "$(grep -Fc "memory: '0.5Gi'" "${bicep}")" == "3" ]] ||
  fail "API, worker, and migration must remain at 0.25 CPU / 0.5Gi"
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
assert_contains "${secret_bicep}" 'scope:[[:space:]]*(databaseSecret|encryptionSecret|hashSecret|encryptionKeysSecret|hashKeysSecret|smtpUsernameSecret|smtpPasswordSecret|vapidPrivateKeySecret)' "secret permissions must use secret-level scopes"
assert_contains "${bicep}" "name: 'VAPID_PRIVATE_KEY', secretRef: 'vapid-private-key'" "worker must receive VAPID private key from Key Vault"
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

assert_contains "${workflow}" '(?s)  deploy:\n.*?outputs:\n      commit: \$\{\{ steps\.release_outputs\.outputs\.commit \}\}\n      image: \$\{\{ steps\.release_outputs\.outputs\.image \}\}' "deploy must expose the production commit and image"
assert_contains "${workflow}" '(?s)  plan:\n.*?Validate release configuration.*?\[\[ "\$GITHUB_REF" == "refs/heads/main" \]\]' "release planning must reject non-main refs before Azure login"
assert_contains "${workflow}" '(?s)  publish_openapi:\n.*?needs: deploy\n.*?environment: production\n.*?contents: read\n      id-token: write' "docs publication must depend on the production deploy and use production OIDC"
assert_contains "${workflow}" 'CONTAINER:[[:space:]]*api-docs-notification-api' "docs publication must use the notification container"
assert_contains "${workflow}" 'RELEASE_COMMIT:[[:space:]]*\$\{\{ needs\.deploy\.outputs\.commit \}\}' "docs publication must consume the deployed commit"
assert_contains "${workflow}" 'RELEASE_IMAGE:[[:space:]]*\$\{\{ needs\.deploy\.outputs\.image \}\}' "docs publication must consume the deployed image"
assert_contains "${workflow}" 'inputs\.fail_openapi_before_pointer && github\.run_attempt == 1' "failure injection must apply only to the first workflow attempt"

publish_job="$(sed -n '/^  publish_openapi:/,$p' "${workflow}")"
printf '%s\n' "${publish_job}" | grep -q 'specs/${GITHUB_SHA}/openapi.yaml' || fail "immutable spec path is missing"
printf '%s\n' "${publish_job}" | grep -q -- '--overwrite false' || fail "immutable spec upload must reject overwrite"
printf '%s\n' "${publish_job}" | grep -q -- '--name current.json' || fail "current pointer upload is missing"
printf '%s\n' "${publish_job}" | grep -q -- '--overwrite true' || fail "current pointer must be replaceable"

workflow_body="$(sed -n '/^          spec_blob="specs\//,$p' "${workflow}" | sed 's/^          //')"
run_openapi_publication_case() {
  local pointer_json="$1"
  local candidate_run_id="$2"
  local expected="$3"
  local failure_injection="${4:-false}"
  local spec_fixture="${5:-missing}"
  local case_dir
  case_dir="$(mktemp -d)"
  mkdir -p "${case_dir}/pointer"
  ln -s "${repo_root}/docs/openapi.yaml" "${case_dir}/docs-openapi.yaml"
  if [[ "${pointer_json}" != missing ]]; then
    printf '%s\n' "${pointer_json}" > "${case_dir}/pointer/current.json"
    cp "${case_dir}/pointer/current.json" "${case_dir}/expected-current.json"
  fi
  case "${spec_fixture}" in
    identical)
      mkdir -p "${case_dir}/blobs/specs/0123456789abcdef0123456789abcdef01234567"
      cp "${repo_root}/docs/openapi.yaml" "${case_dir}/blobs/specs/0123456789abcdef0123456789abcdef01234567/openapi.yaml"
      ;;
    different)
      mkdir -p "${case_dir}/blobs/specs/0123456789abcdef0123456789abcdef01234567"
      printf 'different spec\n' > "${case_dir}/blobs/specs/0123456789abcdef0123456789abcdef01234567/openapi.yaml"
      ;;
  esac

  local output status
  if output="$(POINTER_CASE_DIR="${case_dir}" WORKFLOW_BODY="${workflow_body}" GITHUB_RUN_ID="${candidate_run_id}" GITHUB_SHA=0123456789abcdef0123456789abcdef01234567 GITHUB_REPOSITORY=HallelujahHomeChurch/notification-api RELEASE_COMMIT=0123456789abcdef0123456789abcdef01234567 RELEASE_IMAGE=alive.azurecr.io/alive/notification-api@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef FAIL_OPENAPI_BEFORE_POINTER="${failure_injection}" bash -e -c '
    az() {
      command="$1 $2 $3"
      name=""
      file=""
      overwrite=""
      while [ "$#" -gt 0 ]; do
        case "$1" in
          --name) name="$2"; shift 2 ;;
          --file) file="$2"; shift 2 ;;
          --overwrite) overwrite="$2"; shift 2 ;;
          *) shift ;;
        esac
      done
      blob="$POINTER_CASE_DIR/pointer/current.json"
      if [ "$name" != current.json ]; then blob="$POINTER_CASE_DIR/blobs/$name"; fi
      case "$command" in
        "storage blob exists") if [ -f "$blob" ]; then printf true; else printf false; fi ;;
        "storage blob download") cp "$blob" "$file" ;;
        "storage blob upload")
          if [ -e "$blob" ] && [ "$overwrite" = false ]; then return 1; fi
          mkdir -p "$(dirname "$blob")"
          cp "$file" "$blob"
          if [ "$name" = current.json ]; then printf "%s\n" pointer-upload >> "$POINTER_CASE_DIR/uploads"; else printf "%s\n" spec-upload >> "$POINTER_CASE_DIR/uploads"; fi
          ;;
      esac
    }
    cd "$POINTER_CASE_DIR"
    mkdir -p docs
    ln -s ../docs-openapi.yaml docs/openapi.yaml
    eval "$WORKFLOW_BODY"
  ' 2>&1)"; then
    status=0
  else
    status=$?
  fi
  local pointer_uploaded=false
  if [[ -e "${case_dir}/uploads" ]] && grep -Fxq pointer-upload "${case_dir}/uploads"; then pointer_uploaded=true; fi

  case "${expected}" in
    upload)
      [[ "${status}" -eq 0 && "${pointer_uploaded}" == true ]]
      grep -Fq "/runs/${candidate_run_id}\"" "${case_dir}/pointer/current.json"
      ;;
    noop)
      [[ "${status}" -eq 0 && "${pointer_uploaded}" == false ]]
      cmp "${case_dir}/expected-current.json" "${case_dir}/pointer/current.json"
      ;;
    invalid-pointer)
      [[ "${status}" -ne 0 && "${pointer_uploaded}" == false ]]
      cmp "${case_dir}/expected-current.json" "${case_dir}/pointer/current.json"
      grep -Fq 'Invalid existing API docs pointer: expected canonical GitHub workflow run ID' <<< "${output}"
      ;;
    invalid-candidate)
      [[ "${status}" -ne 0 && "${pointer_uploaded}" == false ]]
      grep -Fq 'Invalid GITHUB_RUN_ID: expected canonical positive decimal' <<< "${output}"
      ;;
    pre-pointer-failure)
      [[ "${status}" -ne 0 && "${pointer_uploaded}" == false ]]
      [[ ! -e "${case_dir}/pointer/current.json" ]]
      grep -Fq 'Requested failure before API docs pointer upload' <<< "${output}"
      ;;
    spec-idempotent)
      [[ "${status}" -eq 0 && "${pointer_uploaded}" == true ]]
      [[ ! -e "${case_dir}/uploads" ]] || ! grep -Fxq spec-upload "${case_dir}/uploads"
      ;;
    spec-mismatch)
      [[ "${status}" -ne 0 && "${pointer_uploaded}" == false ]]
      cmp "${case_dir}/expected-current.json" "${case_dir}/pointer/current.json"
      grep -Fq 'Existing OpenAPI spec hash does not match' <<< "${output}"
      ;;
    pre-pointer-preserve)
      [[ "${status}" -ne 0 && "${pointer_uploaded}" == false ]]
      cmp "${case_dir}/expected-current.json" "${case_dir}/pointer/current.json"
      grep -Fq 'Requested failure before API docs pointer upload' <<< "${output}"
      ;;
  esac
  rm -rf "${case_dir}"
}

valid_pointer='{"releaseUrl":"https://github.com/HallelujahHomeChurch/notification-api/actions/runs/20"}'
run_openapi_publication_case missing 20 upload
run_openapi_publication_case missing 20 pre-pointer-failure true
run_openapi_publication_case "${valid_pointer}" 21 spec-idempotent false identical
run_openapi_publication_case "${valid_pointer}" 21 spec-mismatch false different
run_openapi_publication_case "${valid_pointer}" 21 pre-pointer-preserve true
run_openapi_publication_case "${valid_pointer}" 19 noop
run_openapi_publication_case "${valid_pointer}" 20 noop
run_openapi_publication_case "${valid_pointer}" 21 upload
run_openapi_publication_case '{' 22 invalid-pointer
run_openapi_publication_case '{}' 22 invalid-pointer
run_openapi_publication_case '{"releaseUrl":null}' 22 invalid-pointer
run_openapi_publication_case '{"releaseUrl":"https://github.com/HallelujahHomeChurch/notification-api/actions/runs/09"}' 22 invalid-pointer
run_openapi_publication_case '{"releaseUrl":"https://github.com/HallelujahHomeChurch/notification-api/actions/runs/0"}' 22 invalid-pointer
run_openapi_publication_case '{"releaseUrl":"https://github.com/HallelujahHomeChurch/notification-api/actions/runs/99999999999999999999"}' 100000000000000000000 upload
run_openapi_publication_case missing 0 invalid-candidate
run_openapi_publication_case missing 01 invalid-candidate

deploy_line="$(grep -n '^  deploy:' "${workflow}" | cut -d: -f1)"
publish_line="$(grep -n '^  publish_openapi:' "${workflow}" | cut -d: -f1)"
smoke_line="$(grep -n 'name: Verify Dapr readiness through API gateway' "${workflow}" | cut -d: -f1)"
outputs_line="$(grep -n 'id: release_outputs' "${workflow}" | cut -d: -f1)"
rollback_line="$(grep -n 'name: Roll back runtime after failed deployment verification' "${workflow}" | cut -d: -f1)"
guard_line="$(grep -nF 'pointer_exists="$(az storage blob exists' "${workflow}" | cut -d: -f1)"
guard_exit_line="$(awk '/skipping stale or rerun publication/ { getline; if ($0 ~ /^[[:space:]]*exit 0$/) print NR }' "${workflow}")"
pointer_upload_line="$(awk '/az storage blob upload/ { upload = 1 } upload && /--file current.json/ { print NR; exit }' "${workflow}")"
[[ "${smoke_line}" -lt "${outputs_line}" && "${outputs_line}" -lt "${rollback_line}" ]]
[[ "${deploy_line}" -lt "${publish_line}" && "${guard_line}" -lt "${guard_exit_line}" && "${guard_exit_line}" -lt "${pointer_upload_line}" ]]

echo "release static test ok"
