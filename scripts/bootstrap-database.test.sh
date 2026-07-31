#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

env_file="$tmp_dir/.env.json"
admin_password='admin-secret-must-not-leak'
asset_password='asset-secret-must-not-leak'
printf '{"PG_ADMIN_PASSWORD":"%s","ASSET_DB_PASSWORD":"%s"}\n' \
  "$admin_password" "$asset_password" >"$env_file"
chmod 0600 "$env_file"

first_output="$(
  HHC_ENV_FILE="$env_file" \
  NOTIFICATION_BOOTSTRAP_DRY_RUN=1 \
  "$repo_root/scripts/bootstrap-database.sh" 2>&1
)"
first_values="$(jq -c '{NOTIFICATION_DB_PASSWORD,NOTIFICATION_DATA_ENCRYPTION_KEY,NOTIFICATION_HASH_KEY}' "$env_file")"
second_output="$(
  HHC_ENV_FILE="$env_file" \
  NOTIFICATION_BOOTSTRAP_DRY_RUN=1 \
  "$repo_root/scripts/bootstrap-database.sh" 2>&1
)"
second_values="$(jq -c '{NOTIFICATION_DB_PASSWORD,NOTIFICATION_DATA_ENCRYPTION_KEY,NOTIFICATION_HASH_KEY}' "$env_file")"
test "$first_values" = "$second_values"
output="${first_output}"$'\n'"${second_output}"

test "$(stat -f '%Lp' "$env_file" 2>/dev/null || stat -c '%a' "$env_file")" = "600"
test "$(jq -r '.NOTIFICATION_DB_PASSWORD | length' "$env_file")" = "48"
test "$(jq -r '.NOTIFICATION_DATA_ENCRYPTION_KEY' "$env_file" | base64 --decode | wc -c | tr -d ' ')" = "32"
test "$(jq -r '.NOTIFICATION_HASH_KEY | length' "$env_file")" -ge "32"
test "$(jq -r '.NOTIFICATION_DB_PASSWORD' "$env_file")" != "$admin_password"

grep -q 'host=172.16.68.4' <<<"$output"
grep -q 'runtime-host=hhc-pg.postgres.database.azure.com' <<<"$output"
grep -q 'database=notification' <<<"$output"
grep -q 'role=notification' <<<"$output"
grep -q 'sslmode=require' <<<"$output"
grep -q 'key-vault-secret=notification-database-url' <<<"$output"
grep -q 'key-vault-secret=notification-data-encryption-key' <<<"$output"
grep -q 'key-vault-secret=notification-hash-key' <<<"$output"

patterns_file="$tmp_dir/secrets"
jq -jr '.PG_ADMIN_PASSWORD, "\n", .ASSET_DB_PASSWORD, "\n",
  .NOTIFICATION_DB_PASSWORD, "\n", .NOTIFICATION_DATA_ENCRYPTION_KEY, "\n",
  .NOTIFICATION_HASH_KEY, "\n"' "$env_file" >"$patterns_file"
chmod 0600 "$patterns_file"
if grep -Fq -f "$patterns_file" <<<"$output"; then
  echo "bootstrap output leaked a password" >&2
  exit 1
fi

echo "bootstrap database self-test passed"
