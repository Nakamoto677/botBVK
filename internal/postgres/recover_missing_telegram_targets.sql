-- Retry only the failures that are now recoverable. Other failed operations
-- still require investigation by an administrator.
UPDATE bvk_outbox
SET state='pending', lease_until=NULL, available_at=now()
WHERE state='failed'
  AND (
    (kind='close_topic' AND (
      lower(last_error) LIKE '%message thread not found%'
      OR lower(last_error) LIKE '%topic not found%'
      OR lower(last_error) LIKE '%topic_id_invalid%'
      OR lower(last_error) LIKE '%message_thread_id_invalid%'
    ))
    OR (kind='refresh_card' AND lower(last_error) LIKE '%message to edit not found%')
  );
