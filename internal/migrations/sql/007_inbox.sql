CREATE TABLE inbox_recipient_state (
    recipient_account_id uuid PRIMARY KEY,
    committed_sequence bigint NOT NULL DEFAULT 0 CHECK (committed_sequence >= 0)
);
CREATE TABLE notification_inbox (
    id uuid PRIMARY KEY,
    recipient_account_id uuid NOT NULL REFERENCES inbox_recipient_state(recipient_account_id),
    recipient_sequence bigint NOT NULL CHECK (recipient_sequence > 0),
    producer text NOT NULL CHECK (producer IN ('operations-api','engagement-api')),
    event_type text NOT NULL CHECK (event_type IN ('org_join.requested','org_join.approved','org_join.rejected','org_join.cancelled','recording.published','bulletin.published')),
    logical_notification_key text NOT NULL,
    resource jsonb NOT NULL,
    event_version bigint NOT NULL CHECK (event_version > 0),
    occurred_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    read_at timestamptz,
    UNIQUE (producer,logical_notification_key,recipient_account_id),
    UNIQUE (recipient_account_id,recipient_sequence),
    CHECK (expires_at = occurred_at + interval '2160 hours')
);
CREATE INDEX notification_inbox_page_idx ON notification_inbox(recipient_account_id,occurred_at DESC,id DESC);
CREATE INDEX notification_inbox_unread_idx ON notification_inbox(recipient_account_id,occurred_at DESC,id DESC) WHERE read_at IS NULL;
CREATE INDEX notification_inbox_expiry_idx ON notification_inbox(expires_at,id);

CREATE INDEX account_cleanup_operations_subject_idx ON account_cleanup_operations(subject_ref);
