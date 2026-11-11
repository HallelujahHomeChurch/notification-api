#!/usr/bin/env bash
set -euo pipefail

: "${RESOURCE_GROUP:?RESOURCE_GROUP is required}"
: "${NOTIFICATION_VAULT_NAME:?NOTIFICATION_VAULT_NAME is required}"

role_id="/subscriptions/$(az account show --query id -o tsv)/providers/Microsoft.Authorization/roleDefinitions/4633458b-17de-408a-b874-0445c86b69e6"
vault_json="$(az keyvault show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$NOTIFICATION_VAULT_NAME" \
  -o json --only-show-errors)"
vault_id="$(jq -r '.id' <<<"$vault_json")"
subnet_id="$(az network vnet subnet show \
  --resource-group "$RESOURCE_GROUP" \
  --vnet-name alive-vnet \
  --name aca \
  --query id -o tsv --only-show-errors)"

jq -e --arg subnet "$subnet_id" '
  .properties.enableRbacAuthorization == true
  and .properties.enablePurgeProtection == true
  and .properties.publicNetworkAccess == "Enabled"
  and .properties.networkAcls.bypass == "AzureServices"
  and .properties.networkAcls.defaultAction == "Deny"
  and (.properties.accessPolicies | length) == 0
  and (.properties.networkAcls.ipRules | length) == 0
  and (.properties.networkAcls.virtualNetworkRules | length) == 1
  and (
    .properties.networkAcls.virtualNetworkRules[0].id | ascii_downcase
  ) == ($subnet | ascii_downcase)
' <<<"$vault_json" >/dev/null

api_principal="$(az identity show -g "$RESOURCE_GROUP" -n notification-api-identity --query principalId -o tsv)"
worker_principal="$(az identity show -g "$RESOURCE_GROUP" -n notification-worker-identity --query principalId -o tsv)"
migrate_principal="$(az identity show -g "$RESOURCE_GROUP" -n notification-migrate-identity --query principalId -o tsv)"

verify_secret() {
  local secret="$1"
  local secret_id="${vault_id}/secrets/${secret}"

  az resource show --ids "$secret_id" --api-version 2024-11-01 \
    --query properties.attributes.enabled -o tsv --only-show-errors | grep -qx true
}

verify_assignments() {
  local principal="$1"
  local allow_retiring_smtp="$2"
  shift 2
  local allowed
  allowed="$(printf '%s\n' "$@" | jq -R -s '
    split("\n")[:-1] | map(split("|") | map(ascii_downcase)) | sort
  ')"
  az role assignment list --assignee-object-id "$principal" --all \
    -o json --only-show-errors |
    jq -e --arg vault "$vault_id" --arg role "$role_id" --argjson allowed "$allowed" \
      --argjson allow_retiring_smtp "$allow_retiring_smtp" '
      ($vault | ascii_downcase) as $vault
      | [.[] | {
          scope: (.scope | ascii_downcase),
          role: (.roleDefinitionId | ascii_downcase)
        }
        | select(
            . as $assignment
            | $assignment.scope == $vault
            or ($vault | startswith($assignment.scope + "/"))
            or ($assignment.scope | startswith($vault + "/"))
          )
        | select((
            $allow_retiring_smtp
            and .role == ($role | ascii_downcase)
            and (.scope == ($vault + "/secrets/notification-smtp-username")
              or .scope == ($vault + "/secrets/notification-smtp-password"))
          ) | not)
        | [.scope, .role]
      ] | sort == $allowed
    ' >/dev/null
}

for secret in \
  notification-database-url \
  notification-migrate-database-url \
  notification-data-encryption-key \
  notification-hash-key \
  notification-encryption-keys-json \
  notification-hash-keys-json \
  notification-vapid-private-key
do
  verify_secret "$secret"
done

api_assignments=(
  "${vault_id}/secrets/notification-database-url|${role_id}"
  "${vault_id}/secrets/notification-data-encryption-key|${role_id}"
  "${vault_id}/secrets/notification-hash-key|${role_id}"
  "${vault_id}/secrets/notification-encryption-keys-json|${role_id}"
  "${vault_id}/secrets/notification-hash-keys-json|${role_id}"
)
status_scope="${vault_id}/secrets/notification-engagement-status-token"
if status_enabled="$(az resource show --ids "$status_scope" --api-version 2024-11-01 \
  --query properties.attributes.enabled -o tsv --only-show-errors 2>/dev/null)"; then
  if [[ "$status_enabled" != true ]]; then
    echo "Notification status secret is disabled" >&2
    exit 1
  fi
  api_assignments+=("${status_scope}|${role_id}")
else
  status_result=$?
  if [[ "$status_result" != 3 ]]; then
    echo "Unable to verify Notification status secret metadata" >&2
    exit 1
  fi
fi

verify_private_write() {
  local secret="$1"
  local version="$2"
  [[ -n "$version" ]] || return 0

  if [[ ! "$version" =~ ^[0-9a-f]{32}$ ]]; then
    echo "Invalid pinned version for $secret" >&2
    exit 1
  fi
  local scope="${vault_id}/secrets/${secret}" uri
  uri="$(az resource show --ids "$scope" --api-version 2024-11-01 \
    --query properties.secretUriWithVersion -o tsv --only-show-errors)"
  if [[ "$uri" != "https://${NOTIFICATION_VAULT_NAME}.vault.azure.net/secrets/${secret}/${version}" ]]; then
    echo "Private write credential version did not match for $secret" >&2
    exit 1
  fi
  verify_secret "$secret"
  api_assignments+=("${scope}|${role_id}")
}

verify_private_write notification-account-private-token "${NOTIFICATION_ACCOUNT_PRIVATE_SECRET_VERSION:-}"
verify_private_write notification-engagement-send-token "${NOTIFICATION_ENGAGEMENT_SEND_SECRET_VERSION:-}"
verify_assignments "$api_principal" false "${api_assignments[@]}"
worker_assignments=(
  "${vault_id}/secrets/notification-database-url|${role_id}"
  "${vault_id}/secrets/notification-data-encryption-key|${role_id}"
  "${vault_id}/secrets/notification-encryption-keys-json|${role_id}"
  "${vault_id}/secrets/notification-vapid-private-key|${role_id}"
)
if [[ -n "${NOTIFICATION_WORKER_ELIGIBILITY_SECRET_VERSION:-}" ]]; then
  if [[ ! "${NOTIFICATION_WORKER_ELIGIBILITY_SECRET_VERSION}" =~ ^[0-9a-f]{32}$ ]]; then
    echo "Invalid Notification worker eligibility credential version" >&2
    exit 1
  fi
  eligibility_scope="${vault_id}/secrets/notification-worker-eligibility-token"
  eligibility_uri="$(az resource show --ids "$eligibility_scope" --api-version 2024-11-01 \
    --query 'properties.secretUriWithVersion' -o tsv --only-show-errors)"
  if [[ "$eligibility_uri" != "https://${NOTIFICATION_VAULT_NAME}.vault.azure.net/secrets/notification-worker-eligibility-token/${NOTIFICATION_WORKER_ELIGIBILITY_SECRET_VERSION}" ]]; then
    echo "Notification worker eligibility credential version did not match" >&2
    exit 1
  fi
  verify_secret notification-worker-eligibility-token
  worker_assignments+=("${eligibility_scope}|${role_id}")
fi
# Terraform revokes these two exact worker grants after the runtime bindings are gone.
verify_assignments "$worker_principal" true "${worker_assignments[@]}"
verify_assignments "$migrate_principal" false \
  "${vault_id}/secrets/notification-database-url|${role_id}" \
  "${vault_id}/secrets/notification-migrate-database-url|${role_id}"

echo "notification secret scope is ready"
