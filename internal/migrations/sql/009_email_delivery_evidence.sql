ALTER TABLE notification_email_attempts
 ADD COLUMN provider_request_id varchar(128);

CREATE TABLE notification_email_delivery_events (
 source varchar(32) NOT NULL,
 event_key text NOT NULL CHECK (event_key ~ '^[a-f0-9]{64}$'),
 delivery_id uuid NOT NULL REFERENCES notification_deliveries(id) ON DELETE CASCADE,
 outcome text NOT NULL CHECK (outcome IN ('delivered','bounced')),
 event_at timestamptz NOT NULL,
 PRIMARY KEY(source,event_key)
);
CREATE INDEX notification_email_delivery_events_delivery_idx ON notification_email_delivery_events(delivery_id);

CREATE TABLE notification_email_feedback_cursors (
 source varchar(32) NOT NULL,
 mailbox_id uuid NOT NULL,
 cursor_ciphertext bytea NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(source,mailbox_id)
);
