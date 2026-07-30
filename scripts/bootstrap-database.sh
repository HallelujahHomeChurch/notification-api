#!/usr/bin/env bash
set -euo pipefail

env_file="${HHC_ENV_FILE:-/Users/rayselfs/Projects/hhc/.env.json}"
host="${NOTIFICATION_DB_HOST:-172.16.68.4}"
port="${NOTIFICATION_DB_PORT:-5432}"
database="notification"
role="notification"
tmp_env=""
generated_file=""
trap 'rm -f "${tmp_env:-}" "${generated_file:-}"' EXIT

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

encoded_password="$(printf '%s' "$notification_password" | jq -sRr @uri)"
database_url="postgres://notification:${encoded_password}@${host}:${port}/notification?sslmode=require"
encryption_keys_json="$(jq -r '.NOTIFICATION_ENCRYPTION_KEYS_JSON // empty' "$env_file")"
hash_keys_json="$(jq -r '.NOTIFICATION_HASH_KEYS_JSON // empty' "$env_file")"
[[ -n "$encryption_keys_json" ]] || encryption_keys_json="$(jq -nc --arg key "$data_encryption_key" '{"legacy-v1":$key}')"
[[ -n "$hash_keys_json" ]] || hash_keys_json="$(jq -nc --arg key "$hash_key" '{"legacy-v1":$key}')"
for keyring in "$encryption_keys_json" "$hash_keys_json"; do
  stream_entries="$(jq -n --stream \
    '[inputs | select(length == 2 and (.[0] | length) == 1)] | length' <<<"$keyring")"
  test "$stream_entries" = "$(jq 'length' <<<"$keyring")" || exit 1
done
jq -e 'type == "object" and length > 0 and all(to_entries[];
  (.key | type == "string" and test("\\S")) and (.value | type == "string"))' \
  <<<"$encryption_keys_json" >/dev/null
jq -e 'type == "object" and length > 0 and all(to_entries[];
  (.key | type == "string" and test("\\S")) and (.value | type == "string"))' \
  <<<"$hash_keys_json" >/dev/null
while IFS= read -r key; do
  [[ "$key" =~ ^[A-Za-z0-9+/]{43}=$ ]] || exit 1
  test "$(printf '%s' "$key" | openssl base64 -d -A | wc -c | tr -d ' ')" = "32" || exit 1
done < <(jq -r '.[]' <<<"$encryption_keys_json")
while IFS= read -r key; do
  test "$(printf '%s' "$key" | wc -c | tr -d ' ')" -ge 32 || exit 1
done < <(jq -r '.[]' <<<"$hash_keys_json")
active_encryption_key_id="$(jq -r '.NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID // "legacy-v1"' "$env_file")"
active_hash_key_id="$(jq -r '.NOTIFICATION_ACTIVE_HASH_KEY_ID // "legacy-v1"' "$env_file")"
jq -e --arg id "$active_encryption_key_id" 'has($id)' <<<"$encryption_keys_json" >/dev/null
jq -e --arg id "$active_hash_key_id" 'has($id)' <<<"$hash_keys_json" >/dev/null
jq -e --arg key "$data_encryption_key" '."legacy-v1" == $key' <<<"$encryption_keys_json" >/dev/null
jq -e --arg key "$hash_key" '."legacy-v1" == $key' <<<"$hash_keys_json" >/dev/null
tmp_env="$(mktemp "${env_file}.XXXXXX")"
jq \
  --arg database_url "$database_url" \
  --arg encryption_keys_json "$encryption_keys_json" \
  --arg hash_keys_json "$hash_keys_json" \
  '. + {
    NOTIFICATION_DATABASE_URL: $database_url,
    NOTIFICATION_ENCRYPTION_KEYS_JSON: $encryption_keys_json,
    NOTIFICATION_HASH_KEYS_JSON: $hash_keys_json
  }' \
  "$env_file" >"$tmp_env"
chmod 0600 "$tmp_env"
mv "$tmp_env" "$env_file"
tmp_env=""
unset encoded_password database_url encryption_keys_json hash_keys_json
unset active_encryption_key_id active_hash_key_id
unset stream_entries

echo "host=$host"
echo "database=$database"
echo "role=$role"
echo "sslmode=require"
echo "secret-scope-template=infra/secret-scope.bicep"

if [[ "${NOTIFICATION_BOOTSTRAP_DRY_RUN:-0}" == "1" ]]; then
  exit 0
fi

psql_bin="$(command -v psql || true)"
[[ -n "$psql_bin" ]] || psql_bin="/opt/homebrew/opt/libpq/bin/psql"
[[ -x "$psql_bin" ]] || {
  echo "psql is required" >&2
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

unset data_encryption_key hash_key
echo "notification database is ready; deploy the reviewed dedicated secret scope separately"
