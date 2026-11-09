CREATE TABLE notification_graph_email_budget (
 id boolean PRIMARY KEY DEFAULT true CHECK (id),
 next_send_at timestamptz NOT NULL DEFAULT '1970-01-01T00:00:00Z',
 cooldown_until timestamptz NOT NULL DEFAULT '1970-01-01T00:00:00Z',
 throttle_count integer NOT NULL DEFAULT 0 CHECK (throttle_count BETWEEN 0 AND 6)
);
INSERT INTO notification_graph_email_budget(id) VALUES(true);

CREATE INDEX notification_email_attempts_graph_budget_idx
 ON notification_email_attempts(started_at) WHERE provider='graph' AND outcome<>'deferred';
