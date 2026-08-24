-- +migrate Up
-- Bind output-truncation evidence to the exact persisted assistant message.
-- A single agent_summary_run can be reused by multiple HTTP replay attempts;
-- its run-level latch cannot identify which attempt produced the message later
-- selected for save. Storing the run id and flag on agent_message makes that
-- association explicit and prevents both stale false-PARTIAL and unsafe
-- false-COMPLETE verdicts.
--
-- Empty run_id is the rolling-upgrade marker for legacy rows. The save path
-- falls back to agent_summary_run.output_truncated only for those rows. New V2
-- assistant messages always carry run_id plus their attempt-local flag.

ALTER TABLE `agent_message`
    ADD COLUMN `run_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'agent_summary_run that produced this message; empty for legacy rows',
    ADD COLUMN `output_truncated` TINYINT(1) NOT NULL DEFAULT 0 COMMENT 'this assistant deliverable inherited or directly hit finish_reason=length';

-- +migrate Down
ALTER TABLE `agent_message`
    DROP COLUMN `output_truncated`,
    DROP COLUMN `run_id`;
