package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestEvidenceUpsertClause_MySQLUpdatesOnlyMutableColumns(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "gorm:gorm@tcp(localhost:9910)/gorm?charset=utf8&parseTime=True&loc=Local",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run MySQL dialector: %v", err)
	}

	row := model.AgentMessageEvidence{
		UserID: "u", SessionID: "s", Handle: "h", Evidence: `[{"content":"new"}]`,
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0),
	}
	tx := db.Clauses(evidenceUpsertClause()).Create(&row)
	if tx.Error != nil {
		t.Fatalf("build MySQL upsert: %v", tx.Error)
	}

	sql := strings.ReplaceAll(tx.Statement.SQL.String(), "`", "")
	marker := "ON DUPLICATE KEY UPDATE "
	i := strings.Index(sql, marker)
	if i < 0 {
		t.Fatalf("missing MySQL upsert clause: %s", sql)
	}
	updates := sql[i+len(marker):]
	if !strings.Contains(updates, "evidence=VALUES(evidence)") || !strings.Contains(updates, "updated_at=VALUES(updated_at)") {
		t.Fatalf("MySQL upsert does not refresh evidence and updated_at: %s", updates)
	}
	if strings.Contains(updates, "created_at") {
		t.Fatalf("MySQL upsert must preserve created_at, got: %s", updates)
	}
}
