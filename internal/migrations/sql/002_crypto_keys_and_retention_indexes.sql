ALTER TABLE notification_messages
    ADD COLUMN encryption_key_id text NOT NULL DEFAULT 'legacy-v1',
    ADD COLUMN hash_key_id text NOT NULL DEFAULT 'legacy-v1';
