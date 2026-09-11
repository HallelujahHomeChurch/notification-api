ALTER TABLE notification_messages
  ADD COLUMN eligibility_campaign_id uuid,
  ADD COLUMN eligibility_recipient_id uuid,
  ADD CONSTRAINT notification_eligibility_pair CHECK (
    (eligibility_campaign_id IS NULL) = (eligibility_recipient_id IS NULL)
  );

ALTER TABLE notification_messages DROP CONSTRAINT notification_messages_status_check;
ALTER TABLE notification_messages ADD CONSTRAINT notification_messages_status_check
  CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'dead_lettered', 'suppressed'));

ALTER TABLE notification_deliveries DROP CONSTRAINT notification_deliveries_status_check;
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_status_check
  CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'dead_lettered', 'suppressed'));
