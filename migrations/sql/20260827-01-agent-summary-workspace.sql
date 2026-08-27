-- +migrate Up
-- Durable state and idempotency for the unified summary workspace.

ALTER TABLE `agent_message`
    MODIFY COLUMN `session_id` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
    ADD COLUMN `space_id` VARCHAR(64) NOT NULL DEFAULT '' AFTER `id`,
    ADD COLUMN `turn_id` BIGINT NOT NULL DEFAULT 0 AFTER `user_id`,
    ADD COLUMN `result_type` VARCHAR(32) NOT NULL DEFAULT '' AFTER `output_truncated`,
    ADD COLUMN `response_payload_json` JSON NULL AFTER `result_type`,
    ADD COLUMN `scope_version` INT NOT NULL DEFAULT 0 AFTER `response_payload_json`,
    ADD COLUMN `artifact_version` INT NOT NULL DEFAULT 0 AFTER `scope_version`,
    ADD COLUMN `snapshot_version` INT NOT NULL DEFAULT 0 AFTER `artifact_version`,
    ADD COLUMN `parent_message_id` BIGINT NOT NULL DEFAULT 0 AFTER `snapshot_version`,
    ADD COLUMN `saved_task_id` BIGINT NOT NULL DEFAULT 0 AFTER `parent_message_id`,
    ADD KEY `idx_agent_message_workspace_created` (`space_id`, `user_id`, `session_id`, `id`),
    ADD KEY `idx_agent_message_turn` (`turn_id`);

CREATE TABLE `agent_summary_session` (
    `id` BIGINT NOT NULL AUTO_INCREMENT,
    `space_id` VARCHAR(64) NOT NULL,
    `user_id` VARCHAR(64) NOT NULL,
    `session_id` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
    `agent_session_id` VARCHAR(80) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL DEFAULT '',
    `contract_version` VARCHAR(16) NOT NULL DEFAULT '1',
    `state` VARCHAR(32) NOT NULL DEFAULT 'idle',
    `state_version` BIGINT NOT NULL DEFAULT 1,
    `scope_version` INT NOT NULL DEFAULT 1,
    `scope_json` JSON NULL,
    `scope_hash` CHAR(64) NOT NULL DEFAULT '',
    `active_turn_id` BIGINT NOT NULL DEFAULT 0,
    `artifact_version` INT NOT NULL DEFAULT 0,
    `latest_preview_message_id` BIGINT NOT NULL DEFAULT 0,
    `latest_preview_saved_task_id` BIGINT NOT NULL DEFAULT 0,
    `pending_proposal_version` INT NOT NULL DEFAULT 0,
    `pending_proposal_status` VARCHAR(16) NOT NULL DEFAULT '',
    `pending_proposal_token` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL DEFAULT '',
    `pending_proposal_json` JSON NULL,
    `pending_proposal_message_id` BIGINT NOT NULL DEFAULT 0,
    `pending_proposal_scope_version` INT NOT NULL DEFAULT 0,
    `pending_proposal_task_id` BIGINT NOT NULL DEFAULT 0,
    `workflow_task_id` BIGINT NOT NULL DEFAULT 0,
    `workflow_scope` VARCHAR(16) NOT NULL DEFAULT '',
    `workflow_scope_version` INT NOT NULL DEFAULT 0,
    `workflow_started_message_id` BIGINT NOT NULL DEFAULT 0,
    `workflow_terminal_message_id` BIGINT NOT NULL DEFAULT 0,
    `expires_at` DATETIME(3) NULL,
    `created_at` DATETIME(3) NOT NULL,
    `updated_at` DATETIME(3) NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_agent_summary_session` (`space_id`, `user_id`, `session_id`),
    KEY `idx_agent_summary_session_agent_identity` (`user_id`, `agent_session_id`),
    KEY `idx_agent_summary_session_workflow` (`workflow_task_id`),
    KEY `idx_agent_summary_session_expires` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE `agent_summary_turn` (
    `id` BIGINT NOT NULL AUTO_INCREMENT,
    `space_id` VARCHAR(64) NOT NULL,
    `user_id` VARCHAR(64) NOT NULL,
    `session_id` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
    `request_id` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
    `request_hash` CHAR(64) NOT NULL,
    `scope_version` INT NOT NULL,
    `status` VARCHAR(16) NOT NULL,
    `attempt` INT NOT NULL DEFAULT 1,
    `lease_expires_at` DATETIME(3) NULL,
    `run_id` VARCHAR(64) NOT NULL DEFAULT '',
    `response_message_id` BIGINT NOT NULL DEFAULT 0,
    `result_type` VARCHAR(32) NOT NULL DEFAULT '',
    `response_json` JSON NULL,
    `error_code` VARCHAR(64) NOT NULL DEFAULT '',
    `created_at` DATETIME(3) NOT NULL,
    `updated_at` DATETIME(3) NOT NULL,
    `completed_at` DATETIME(3) NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_agent_summary_turn_request` (`space_id`, `user_id`, `session_id`, `request_id`),
    KEY `idx_agent_summary_turn_session` (`session_id`),
    KEY `idx_agent_summary_turn_lease` (`status`, `lease_expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS `agent_summary_turn`;
DROP TABLE IF EXISTS `agent_summary_session`;
ALTER TABLE `agent_message`
    DROP KEY `idx_agent_message_turn`,
    DROP KEY `idx_agent_message_workspace_created`,
    DROP COLUMN `saved_task_id`,
    DROP COLUMN `parent_message_id`,
    DROP COLUMN `snapshot_version`,
    DROP COLUMN `artifact_version`,
    DROP COLUMN `scope_version`,
    DROP COLUMN `response_payload_json`,
    DROP COLUMN `result_type`,
    DROP COLUMN `turn_id`,
    DROP COLUMN `space_id`,
    MODIFY COLUMN `session_id` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL;
