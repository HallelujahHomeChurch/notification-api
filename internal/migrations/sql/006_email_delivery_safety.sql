ALTER TABLE notification_messages ADD COLUMN email_history_complete boolean NOT NULL DEFAULT false;
ALTER TABLE notification_deliveries ADD COLUMN failure_count integer NOT NULL DEFAULT 0 CHECK (failure_count>=0);
UPDATE notification_deliveries SET failure_count=attempt_count;

CREATE TABLE notification_email_attempts (
 delivery_id uuid NOT NULL REFERENCES notification_deliveries(id) ON DELETE CASCADE,
 attempt integer NOT NULL CHECK (attempt>0),
 bulk boolean NOT NULL,
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 finished_at timestamptz,
 outcome text NOT NULL CHECK (outcome IN ('started','deferred','accepted','rejected','unknown')),
 stage text NOT NULL DEFAULT '' CHECK (stage IN ('','validate','dial','deadline','greeting','starttls','auth','mail','recipient','data','message')),
 smtp_code integer NOT NULL DEFAULT 0 CHECK (smtp_code=0 OR smtp_code BETWEEN 200 AND 599),
 enhanced_code text NOT NULL DEFAULT '' CHECK (enhanced_code='' OR enhanced_code ~ '^[245]\.[0-9]{1,3}\.[0-9]{1,3}$'),
 PRIMARY KEY(delivery_id,attempt)
);
CREATE INDEX notification_email_attempts_budget_idx ON notification_email_attempts(started_at) WHERE outcome<>'deferred';

CREATE TABLE notification_email_budget (
 id boolean PRIMARY KEY DEFAULT true CHECK (id),
 next_send_at timestamptz NOT NULL DEFAULT '1970-01-01T00:00:00Z',
 cooldown_until timestamptz NOT NULL DEFAULT '1970-01-01T00:00:00Z',
 throttle_count integer NOT NULL DEFAULT 0 CHECK (throttle_count BETWEEN 0 AND 6)
);
INSERT INTO notification_email_budget(id) VALUES(true);
