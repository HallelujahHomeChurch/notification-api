ALTER TABLE notification_messages
  ADD COLUMN subject_hash text,
  ADD COLUMN subject_hash_key_id text,
  ADD CONSTRAINT notification_messages_subject_hash_pair CHECK (
    (subject_hash IS NULL AND subject_hash_key_id IS NULL)
    OR (subject_hash ~ '^[0-9a-f]{64}$' AND subject_hash_key_id IS NOT NULL)
  );

CREATE INDEX notification_messages_subject_hash_idx
  ON notification_messages(subject_hash_key_id, subject_hash)
  WHERE subject_hash IS NOT NULL;
