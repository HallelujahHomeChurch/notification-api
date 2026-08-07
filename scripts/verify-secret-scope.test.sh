#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

cat >"$tmp_dir/az" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail

vault="/subscriptions/test/resourceGroups/alive/providers/Microsoft.KeyVault/vaults/alive-notification-test"
role="/subscriptions/test/providers/Microsoft.Authorization/roleDefinitions/4633458b-17de-408a-b874-0445c86b69e6"
case "$1 $2" in
  "account show")
    echo test
    ;;
  "keyvault show")
    jq -nc --arg id "$vault" '{
      id:$id,
      properties:{
        enableRbacAuthorization:true,
        enablePurgeProtection:true,
        publicNetworkAccess:"Enabled",
        accessPolicies:[],
        networkAcls:{
          bypass:"AzureServices",
          defaultAction:"Deny",
          ipRules:[],
          virtualNetworkRules:[{
            id:"/subscriptions/test/resourceGroups/alive/providers/Microsoft.Network/virtualNetworks/alive-vnet/subnets/aca"
          }]
        }
      }
    }'
    ;;
  "network vnet")
    echo "/subscriptions/test/resourceGroups/alive/providers/Microsoft.Network/virtualNetworks/alive-vnet/subnets/aca"
    ;;
  "identity show")
    case "$*" in
      *notification-api-identity*) echo api ;;
      *notification-worker-identity*) echo worker ;;
      *notification-migrate-identity*) echo migrate ;;
    esac
    ;;
  "resource show")
    echo true
    ;;
  "role assignment")
    principal=""
    while (($#)); do
      if [[ "$1" == "--assignee-object-id" ]]; then
        principal="$2"
        break
      fi
      shift
    done
    secrets=()
    case "$principal" in
      api)
        secrets=(notification-database-url notification-data-encryption-key notification-hash-key notification-encryption-keys-json notification-hash-keys-json)
        ;;
      worker)
		secrets=(notification-database-url notification-data-encryption-key notification-encryption-keys-json notification-smtp-username notification-smtp-password notification-vapid-private-key)
        ;;
      migrate)
        secrets=(notification-database-url)
        ;;
    esac
    jq -nc --arg vault "$vault" --arg role "$role" --argjson extra "${MOCK_EXTRA_ROLE:-false}" \
      --args '
        [$ARGS.positional[] | {
          scope:($vault + "/secrets/" + .),
          roleDefinitionId:$role
        }]
        + if $extra and ($ARGS.positional | length) > 1 then [{
            scope:($vault + "/secrets/" + $ARGS.positional[0]),
            roleDefinitionId:"/subscriptions/test/providers/Microsoft.Authorization/roleDefinitions/admin"
          }] else [] end
      ' "${secrets[@]}"
    ;;
esac
MOCK
chmod +x "$tmp_dir/az"

PATH="$tmp_dir:$PATH" RESOURCE_GROUP=alive NOTIFICATION_VAULT_NAME=alive-notification-test \
  "$repo_root/scripts/verify-secret-scope.sh" >/dev/null
if PATH="$tmp_dir:$PATH" MOCK_EXTRA_ROLE=true RESOURCE_GROUP=alive \
  NOTIFICATION_VAULT_NAME=alive-notification-test \
  "$repo_root/scripts/verify-secret-scope.sh" >/dev/null 2>&1; then
  echo "secret preflight accepted an extra secret role" >&2
  exit 1
fi

echo "secret scope self-test passed"
