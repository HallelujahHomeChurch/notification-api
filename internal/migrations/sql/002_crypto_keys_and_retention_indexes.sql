ALTER TABLE notification_messages
    ADD COLUMN encryption_key_id text NOT NULL DEFAULT 'legacy-v1',
    ADD COLUMN hash_key_id text NOT NULL DEFAULT 'legacy-v1',
    ADD COLUMN expires_at timestamptz;

UPDATE notification_messages
SET expires_at = created_at + CASE template_id
    WHEN 'account.verify-email' THEN interval '24 hours'
    WHEN 'account.reset-password' THEN interval '1 hour'
    WHEN 'account.oauth-link-confirmation' THEN interval '15 minutes'
    ELSE interval '15 minutes'
END;

ALTER TABLE notification_messages
    ALTER COLUMN expires_at
        SET DEFAULT (clock_timestamp() + interval '15 minutes'),
    ALTER COLUMN expires_at SET NOT NULL;

CREATE INDEX notification_messages_terminal_idx
    ON notification_messages (terminal_at, id)
    WHERE terminal_at IS NOT NULL;

CREATE INDEX notification_messages_unpurged_terminal_idx
    ON notification_messages (terminal_at, id)
    WHERE terminal_at IS NOT NULL AND payload_purged_at IS NULL;

CREATE INDEX notification_deliveries_message_id_idx
    ON notification_deliveries (message_id);

CREATE INDEX notification_outbox_delivery_id_idx
    ON notification_outbox (delivery_id);
