ALTER TABLE notification_messages ADD COLUMN target_hashes jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(target_hashes)='object');

CREATE TABLE notification_email_feedback (
 sns_message_id uuid PRIMARY KEY,
 delivery_id uuid NOT NULL REFERENCES notification_deliveries(id) ON DELETE CASCADE,
 provider_message_id text NOT NULL,
 event_type text NOT NULL CHECK (event_type IN ('send','delivery','hard_bounce','soft_bounce','complaint','reject','delay','rendering_failure')),
 event_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX notification_email_feedback_delivery_idx ON notification_email_feedback(delivery_id);

-- Minimal address-level safety block, independent of newsletter consent and payload retention.
CREATE TABLE notification_email_suppressions (
 hash_key_id text NOT NULL,
 target_hash text NOT NULL CHECK (target_hash ~ '^[a-f0-9]{64}$'),
 reason text NOT NULL CHECK (reason IN ('hard_bounce','complaint')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(hash_key_id,target_hash)
);
