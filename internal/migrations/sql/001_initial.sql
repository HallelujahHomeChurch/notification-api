CREATE TABLE notification_messages (
    id uuid PRIMARY KEY,
    caller_app_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash text NOT NULL,
    template_id text NOT NULL,
    template_version integer NOT NULL CHECK (template_version > 0),
    channel text NOT NULL,
    target_type text NOT NULL,
    target_hash text NOT NULL,
    target_ciphertext bytea NOT NULL,
    payload_ciphertext bytea NOT NULL,
    resource_type text NOT NULL,
    resource_id text NOT NULL,
    status text NOT NULL CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'dead_lettered')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    terminal_at timestamptz,
    payload_purged_at timestamptz,
    UNIQUE (caller_app_id, idempotency_key)
);

CREATE TABLE notification_deliveries (
    id uuid PRIMARY KEY,
    message_id uuid NOT NULL REFERENCES notification_messages(id) ON DELETE CASCADE,
    channel text NOT NULL,
    endpoint_ref text,
    provider text NOT NULL,
    status text NOT NULL CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'dead_lettered')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_expires_at timestamptz,
    sent_at timestamptz,
    provider_message_id text,
    last_error_code text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX notification_deliveries_queued_idx
    ON notification_deliveries (next_attempt_at, created_at)
    WHERE status = 'queued';

CREATE TABLE notification_outbox (
    id uuid PRIMARY KEY,
    delivery_id uuid NOT NULL REFERENCES notification_deliveries(id) ON DELETE CASCADE,
    status text NOT NULL CHECK (status IN ('pending', 'publishing', 'published')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_expires_at timestamptz,
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX notification_outbox_pending_idx
    ON notification_outbox (next_attempt_at, created_at)
    WHERE status = 'pending';

CREATE TABLE notification_rate_limits (
    bucket_key text PRIMARY KEY,
    count bigint NOT NULL CHECK (count >= 0),
    expires_at timestamptz NOT NULL
);

CREATE INDEX notification_rate_limits_expires_at_idx
    ON notification_rate_limits (expires_at);
