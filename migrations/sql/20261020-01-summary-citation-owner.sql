-- +migrate Up
ALTER TABLE summary_result ADD COLUMN citation_owner_id VARCHAR(64) NOT NULL DEFAULT '';
-- Backfill only versions with a server-authenticated optimization save binding.
-- CreatedBy alone is not proof of authorization to the underlying raw messages.
UPDATE summary_result AS r
JOIN summary_agent_save_idempotency AS s
  ON s.result_id = r.id AND s.task_id = r.task_id AND s.result_kind = 'team'
SET r.citation_owner_id = s.user_id
WHERE r.citation_owner_id = '';
-- +migrate Down
ALTER TABLE summary_result DROP COLUMN citation_owner_id;
