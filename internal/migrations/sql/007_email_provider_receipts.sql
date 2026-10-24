-- Additive: historical SMTP attempts keep the existing recovery representation.
ALTER TABLE notification_email_attempts
 ADD COLUMN provider text NOT NULL DEFAULT 'smtp',
 ADD COLUMN provider_message_id text,
 ADD COLUMN provider_accepted_at timestamptz;
