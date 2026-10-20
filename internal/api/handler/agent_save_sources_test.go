//go:build cgo

package handler

import (
	"context"
	"github.com/mattn/go-sqlite3"
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// Existing legacy-save tests need actual authorized IM fixtures now, rather
// than treating a nil IM DB as permission to persist arbitrary source IDs.
func newLegacySaveIMDB(t *testing.T, groups ...string) *gorm.DB {
	t.Helper()
	db := newSummaryWorkspaceIMValidationDB(t)
	enableLegacyIMSQL(t, db)
	for _, id := range groups {
		if err := db.Exec(`INSERT INTO "group" VALUES (?,?,'test-space',1,1)`, id, id).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO group_member VALUES (?,'test-user',1,0,0)`, id).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec(`INSERT INTO group_member SELECT group_no,'test-uid',1,0,0 FROM "group" WHERE space_id='test-space'`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func enableLegacyIMSQL(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Raw(func(raw interface{}) error {
		sqliteConn := raw.(*sqlite3.SQLiteConn)
		if err := sqliteConn.RegisterCollation("utf8mb4_unicode_ci", strings.Compare); err != nil {
			return err
		}
		return sqliteConn.RegisterFunc("CONCAT", func(parts ...string) string { return strings.Join(parts, "") }, true)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacySaveAuthorizesSourcesAndExplicitOrigin(t *testing.T) {
	cases := []struct {
		name, id string
		kind     int
		origin   bool
		want     int
	}{
		{"group", "group-a", 1, false, 200}, {"cross-space", "group-b", 1, false, 404},
		{"private", "private-group", 1, false, 404}, {"dm", "peer-in", 3, false, 200},
		{"dm-outside", "peer-out", 3, false, 404}, {"thread", "group-a____allowed-thread", 2, false, 200},
		{"private-thread", "private-thread", 2, false, 404},
		{"inactive-thread", "group-a____allowed-thread", 2, false, 404}, {"forged-origin", "group-b", 1, true, 404},
		{"inactive", "group-a", 1, false, 404}, {"no-im", "group-a", 1, false, 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupAgentSummaryTestDB(t)
			im := newSummaryWorkspaceIMValidationDB(t)
			enableLegacyIMSQL(t, im)
			if err := im.Exec(`INSERT INTO thread (short_id,name,group_no,status,message_count,creator_uid,updated_at) VALUES ('allowed-thread','Thread','group-a',1,1,'actor',1)`).Error; err != nil {
				t.Fatal(err)
			}
			if tc.name == "inactive" || tc.name == "inactive-thread" {
				if err := im.Exec(`UPDATE group_member SET status=0 WHERE uid='actor' AND group_no='group-a'`).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "no-im" {
				im = nil
			}
			seedAssistantMessage(t, db, "actor", "legacy-auth", "### Summary\nBody")
			h := NewAgentSummaryHandler(db, im, "", "", "", 0, 0)
			body := map[string]interface{}{"session_id": "legacy-auth", "origin_channel_id": "group-a", "origin_channel_type": 1, "sources": []sourceReq{{SourceID: tc.id, SourceType: tc.kind}}}
			if tc.origin {
				body["origin_channel_id"] = tc.id
				body["sources"] = []sourceReq{}
			}
			w := doAgentSave(t, setupAgentSummaryRouter(h), body, map[string]string{"Token": "actor", "X-Space-Id": "space-a"})
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != http.StatusOK {
				for _, table := range []interface{}{&model.SummaryTask{}, &model.SummarySource{}, &model.PersonalResult{}, &model.SummaryAgentSaveIdempotency{}} {
					var count int64
					if err := db.Model(table).Count(&count).Error; err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Fatalf("rejected save persisted %T", table)
					}
				}
				var count int64
				db.Model(&model.AgentMessage{}).Count(&count)
				if count != 1 {
					t.Fatal("rejected save deleted draft")
				}
			}
		})
	}
}
