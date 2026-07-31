#!/usr/bin/env bash
set -euo pipefail

env_file="${HHC_ENV_FILE:-/Users/rayselfs/Projects/hhc/.env.json}"
host="${NOTIFICATION_DB_HOST:-172.16.68.4}"
runtime_host="${NOTIFICATION_RUNTIME_DB_HOST:-hhc-pg.postgres.database.azure.com}"
port="${NOTIFICATION_DB_PORT:-5432}"
database="notification"
role="notification"
vault="${NOTIFICATION_KEY_VAULT:-alive-vault}"
secret_name="notification-database-url"
tmp_env=""
secret_file=""
generated_file=""
trap 'rm -f "${tmp_env:-}" "${secret_file:-}" "${generated_file:-}"' EXIT

for command in jq openssl; do
  command -v "$command" >/dev/null || {
    echo "$command is required" >&2
    exit 1
  }
done

[[ -f "$env_file" ]] || {
  echo "environment file not found: $env_file" >&2
  exit 1
}
chmod 0600 "$env_file"

admin_password="$(jq -er '.PG_ADMIN_PASSWORD' "$env_file")"
notification_password="$(jq -r '.NOTIFICATION_DB_PASSWORD // empty' "$env_file")"
data_encryption_key="$(jq -r '.NOTIFICATION_DATA_ENCRYPTION_KEY // empty' "$env_file")"
hash_key="$(jq -r '.NOTIFICATION_HASH_KEY // empty' "$env_file")"
if [[ -z "$notification_password" || -z "$data_encryption_key" || -z "$hash_key" ]]; then
  [[ -n "$notification_password" ]] || notification_password="$(openssl rand -hex 24)"
  [[ -n "$data_encryption_key" ]] || data_encryption_key="$(openssl rand -base64 32)"
  [[ -n "$hash_key" ]] || hash_key="$(openssl rand -hex 32)"
  tmp_env="$(mktemp "${env_file}.XXXXXX")"
  generated_file="$(mktemp)"
  chmod 0600 "$generated_file"
  printf '{"NOTIFICATION_DB_PASSWORD":"%s","NOTIFICATION_DATA_ENCRYPTION_KEY":"%s","NOTIFICATION_HASH_KEY":"%s"}' \
    "$notification_password" "$data_encryption_key" "$hash_key" >"$generated_file"
  jq --slurpfile generated "$generated_file" '. + $generated[0]' "$env_file" >"$tmp_env"
  chmod 0600 "$tmp_env"
  mv "$tmp_env" "$env_file"
  rm -f "$generated_file"
  generated_file=""
fi

echo "host=$host"
echo "runtime-host=$runtime_host"
echo "database=$database"
echo "role=$role"
echo "sslmode=require"
echo "key-vault-secret=$secret_name"
echo "key-vault-secret=notification-data-encryption-key"
echo "key-vault-secret=notification-hash-key"

if [[ "${NOTIFICATION_BOOTSTRAP_DRY_RUN:-0}" == "1" ]]; then
  exit 0
fi

psql_bin="$(command -v psql || true)"
[[ -n "$psql_bin" ]] || psql_bin="/opt/homebrew/opt/libpq/bin/psql"
[[ -x "$psql_bin" ]] || {
  echo "psql is required" >&2
  exit 1
}
command -v az >/dev/null || {
  echo "az is required" >&2
  exit 1
}

export PGPASSWORD="$admin_password"
export NOTIFICATION_DB_PASSWORD="$notification_password"
"$psql_bin" "host=$host port=$port dbname=postgres user=HHCAdmin sslmode=require" \
  --set=ON_ERROR_STOP=1 <<'SQL'
\getenv notification_password NOTIFICATION_DB_PASSWORD
SELECT format('CREATE ROLE notification LOGIN PASSWORD %L', :'notification_password')
WHERE NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'notification')
\gexec
ALTER ROLE notification LOGIN PASSWORD :'notification_password';
SELECT 'CREATE DATABASE notification OWNER notification'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'notification')
\gexec
\connect notification
ALTER SCHEMA public OWNER TO notification;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO notification;
SQL
unset PGPASSWORD NOTIFICATION_DB_PASSWORD admin_password notification_password

encoded_password="$(jq -j '.NOTIFICATION_DB_PASSWORD' "$env_file" | jq -sRr @uri)"
secret_file="$(mktemp)"
chmod 0600 "$secret_file"
printf 'postgres://notification:%s@%s:%s/notification?sslmode=require' \
  "$encoded_password" "$runtime_host" "$port" >"$secret_file"
unset encoded_password

az keyvault secret set \
  --vault-name "$vault" \
  --name "$secret_name" \
  --file "$secret_file" \
  --content-type 'text/plain' \
  --only-show-errors \
  --output none

for config_secret in notification-data-encryption-key notification-hash-key; do
  case "$config_secret" in
    notification-data-encryption-key) config_value="$data_encryption_key" ;;
    notification-hash-key) config_value="$hash_key" ;;
  esac
  printf '%s' "$config_value" >"$secret_file"
  az keyvault secret set \
    --vault-name "$vault" \
    --name "$config_secret" \
    --file "$secret_file" \
    --content-type 'text/plain' \
    --only-show-errors \
    --output none
done

unset config_value data_encryption_key hash_key
rm -f "$secret_file"
echo "notification database and Key Vault secret are ready"
