package db

import (
	"strings"
	"testing"

	migrationsql "github.com/Mininglamp-OSS/octo-smart-summary/migrations/sql"
)

func TestAgentSummaryWorkspaceMigrationDefinesDurableContract(t *testing.T) {
	raw, err := migrationsql.FS.ReadFile("20260827-01-agent-summary-workspace.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(raw)
	for _, required := range []string{
		"CREATE TABLE `agent_summary_session`",
		"CREATE TABLE `agent_summary_turn`",
		"UNIQUE KEY `uk_agent_summary_session` (`space_id`, `user_id`, `session_id`)",
		"KEY `idx_agent_summary_session_agent_identity` (`user_id`, `agent_session_id`)",
		"`agent_session_id` VARCHAR(80)",
		"UNIQUE KEY `uk_agent_summary_turn_request` (`space_id`, `user_id`, `session_id`, `request_id`)",
		"MODIFY COLUMN `session_id` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL",
		"MODIFY COLUMN `session_id` VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL",
		"`pending_proposal_json` JSON NULL",
		"`response_json` JSON NULL",
		"COLLATE utf8mb4_0900_bin",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("workspace migration is missing %q", required)
		}
	}
}
