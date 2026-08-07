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
  shift
  local allowed
  allowed="$(printf '%s\n' "$@" | jq -R -s '
    split("\n")[:-1] | map(split("|") | map(ascii_downcase)) | sort
  ')"
  az role assignment list --assignee-object-id "$principal" --all \
    -o json --only-show-errors |
    jq -e --arg vault "$vault_id" --argjson allowed "$allowed" '
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
        | [.scope, .role]
      ] | sort == $allowed
    ' >/dev/null
}

for secret in \
  notification-database-url \
  notification-data-encryption-key \
  notification-hash-key \
  notification-encryption-keys-json \
  notification-hash-keys-json \
  notification-smtp-username \
  notification-smtp-password \
  notification-vapid-private-key
do
  verify_secret "$secret"
done

verify_assignments "$api_principal" \
  "${vault_id}/secrets/notification-database-url|${role_id}" \
  "${vault_id}/secrets/notification-data-encryption-key|${role_id}" \
  "${vault_id}/secrets/notification-hash-key|${role_id}" \
  "${vault_id}/secrets/notification-encryption-keys-json|${role_id}" \
  "${vault_id}/secrets/notification-hash-keys-json|${role_id}"
verify_assignments "$worker_principal" \
  "${vault_id}/secrets/notification-database-url|${role_id}" \
  "${vault_id}/secrets/notification-data-encryption-key|${role_id}" \
  "${vault_id}/secrets/notification-encryption-keys-json|${role_id}" \
  "${vault_id}/secrets/notification-smtp-username|${role_id}" \
  "${vault_id}/secrets/notification-smtp-password|${role_id}" \
  "${vault_id}/secrets/notification-vapid-private-key|${role_id}"
verify_assignments "$migrate_principal" \
  "${vault_id}/secrets/notification-database-url|${role_id}"

echo "notification secret scope is ready"
