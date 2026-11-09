ALTER TABLE notification_email_feedback_cursors
 ADD COLUMN encryption_key_id text;

ALTER TABLE notification_deliveries
 ADD COLUMN graph_mailbox_id uuid,
 ADD CONSTRAINT notification_graph_mailbox_provider CHECK (graph_mailbox_id IS NULL OR provider='graph');
