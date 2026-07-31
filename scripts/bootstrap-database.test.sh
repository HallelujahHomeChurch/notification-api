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
test "$(jq -r '.NOTIFICATION_DATABASE_URL | startswith("postgres://notification:")' "$env_file")" = "true"
test "$(jq -r '.NOTIFICATION_ENCRYPTION_KEYS_JSON | fromjson | has("legacy-v1")' "$env_file")" = "true"
test "$(jq -r '.NOTIFICATION_HASH_KEYS_JSON | fromjson | has("legacy-v1")' "$env_file")" = "true"

legacy_encryption="$(jq -r '.NOTIFICATION_DATA_ENCRYPTION_KEY' "$env_file")"
legacy_hash="$(jq -r '.NOTIFICATION_HASH_KEY' "$env_file")"
rotated_encryption="$(jq -nc \
  --arg legacy "$legacy_encryption" \
  --arg v2 "$(openssl rand -base64 32)" \
  '{"legacy-v1":$legacy,"v2":$v2}')"
rotated_hash="$(jq -nc \
  --arg legacy "$legacy_hash" \
  --arg v2 "$(openssl rand -hex 32)" \
  '{"legacy-v1":$legacy,"v2":$v2}')"
tmp_env="$tmp_dir/rotated.json"
jq \
  --arg encryption "$rotated_encryption" \
  --arg hash "$rotated_hash" \
  '. + {
    NOTIFICATION_ENCRYPTION_KEYS_JSON: $encryption,
    NOTIFICATION_HASH_KEYS_JSON: $hash,
    NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID: "v2",
    NOTIFICATION_ACTIVE_HASH_KEY_ID: "v2"
  }' "$env_file" >"$tmp_env"
mv "$tmp_env" "$env_file"
HHC_ENV_FILE="$env_file" NOTIFICATION_BOOTSTRAP_DRY_RUN=1 \
  "$repo_root/scripts/bootstrap-database.sh" >/dev/null
test "$(jq -r '.NOTIFICATION_ENCRYPTION_KEYS_JSON' "$env_file")" = "$rotated_encryption"
test "$(jq -r '.NOTIFICATION_HASH_KEYS_JSON' "$env_file")" = "$rotated_hash"

invalid_env="$tmp_dir/invalid.json"
jq '.NOTIFICATION_ENCRYPTION_KEYS_JSON = "{\"legacy-v1\":\"mismatch\",\"v2\":\"mismatch\"}"' \
  "$env_file" >"$invalid_env"
if HHC_ENV_FILE="$invalid_env" NOTIFICATION_BOOTSTRAP_DRY_RUN=1 \
  "$repo_root/scripts/bootstrap-database.sh" >/dev/null 2>&1; then
  echo "bootstrap accepted an invalid encryption keyring" >&2
  exit 1
fi
invalid_base64="${legacy_encryption}!"
jq --arg key "$invalid_base64" \
  '.NOTIFICATION_DATA_ENCRYPTION_KEY = $key
   | .NOTIFICATION_ENCRYPTION_KEYS_JSON = (
       .NOTIFICATION_ENCRYPTION_KEYS_JSON | fromjson
       | ."legacy-v1" = $key | tojson
     )' "$env_file" >"$invalid_env"
if HHC_ENV_FILE="$invalid_env" NOTIFICATION_BOOTSTRAP_DRY_RUN=1 \
  "$repo_root/scripts/bootstrap-database.sh" >/dev/null 2>&1; then
  echo "bootstrap accepted non-standard base64" >&2
  exit 1
fi
v2_encryption="$(jq -r '.v2' <<<"$rotated_encryption")"
duplicate_encryption="$(printf \
  '{"legacy-v1":"%s","v2":"%s","v2":"%s"}' \
  "$legacy_encryption" "$v2_encryption" "$v2_encryption")"
jq --arg keyring "$duplicate_encryption" \
  '.NOTIFICATION_ENCRYPTION_KEYS_JSON = $keyring' "$env_file" >"$invalid_env"
if HHC_ENV_FILE="$invalid_env" NOTIFICATION_BOOTSTRAP_DRY_RUN=1 \
  "$repo_root/scripts/bootstrap-database.sh" >/dev/null 2>&1; then
  echo "bootstrap accepted a duplicate key ID" >&2
  exit 1
fi
jq '.NOTIFICATION_HASH_KEYS_JSON = "{\"legacy-v1\":\"short\",\"v2\":\"short\"}"' \
  "$env_file" >"$invalid_env"
if HHC_ENV_FILE="$invalid_env" NOTIFICATION_BOOTSTRAP_DRY_RUN=1 \
  "$repo_root/scripts/bootstrap-database.sh" >/dev/null 2>&1; then
  echo "bootstrap accepted an invalid hash keyring" >&2
  exit 1
fi

grep -q 'host=172.16.68.4' <<<"$output"
grep -q 'database=notification' <<<"$output"
grep -q 'role=notification' <<<"$output"
grep -q 'sslmode=require' <<<"$output"
grep -q 'secret-scope-template=infra/secret-scope.bicep' <<<"$output"
if grep -q 'alive-vault' <<<"$output"; then
  echo "bootstrap output still targets the shared vault" >&2
  exit 1
fi

patterns_file="$tmp_dir/secrets"
jq -jr '.PG_ADMIN_PASSWORD, "\n", .ASSET_DB_PASSWORD, "\n",
  .NOTIFICATION_DB_PASSWORD, "\n", .NOTIFICATION_DATA_ENCRYPTION_KEY, "\n",
  .NOTIFICATION_HASH_KEY, "\n", .NOTIFICATION_DATABASE_URL, "\n",
  .NOTIFICATION_ENCRYPTION_KEYS_JSON, "\n", .NOTIFICATION_HASH_KEYS_JSON, "\n"' \
  "$env_file" >"$patterns_file"
chmod 0600 "$patterns_file"
if grep -Fq -f "$patterns_file" <<<"$output"; then
  echo "bootstrap output leaked a password" >&2
  exit 1
fi

echo "bootstrap database self-test passed"
