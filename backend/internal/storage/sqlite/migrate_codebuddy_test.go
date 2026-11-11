package sqlite

import (
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestMigration20261110143421861AllowsCodebuddy(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261109120000000)
	var before string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'sessions'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(before, "'codebuddy'") {
		t.Fatal("checkpoint already allows codebuddy")
	}
	upTo(t, db, 20261110143421861)
	mustExec(t, db, `INSERT INTO projects (id, path, registered_at) VALUES ('cb-project', '/tmp/cb', CURRENT_TIMESTAMP)`)
	insert := `INSERT INTO sessions (id, project_id, num, harness, created_at, updated_at, activity_last_at) VALUES (?, 'cb-project', ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`
	if _, err := db.Exec(insert, "cb-session", 1, "codebuddy"); err != nil {
		t.Fatalf("insert codebuddy: %v", err)
	}
	if _, err := db.Exec(insert, "unknown", 2, "unknown-agent"); err == nil {
		t.Fatal("unknown harness bypassed the CHECK constraint")
	}
	mustExec(t, db, `UPDATE sessions SET harness = '' WHERE harness = 'codebuddy'`)
	downTo(t, db, 20261109120000000)
	var after string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'sessions'`).Scan(&after); err != nil || after != before {
		t.Fatalf("down migration did not restore the schema: %v", err)
	}
	if _, err := db.Exec(insert, "cb-after", 3, "codebuddy"); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("codebuddy insertion after downgrade = %v", err)
	}
}

func TestMigration20261110143421861RejectsUnrecognizedConstraint(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261109120000000)
	mustExec(t, db, `PRAGMA writable_schema = ON`)
	mustExec(t, db, `UPDATE sqlite_master SET sql = replace(sql, '''fake''))', '''fake'', ''other''))') WHERE type = 'table' AND name = 'sessions'`)
	mustExec(t, db, `PRAGMA writable_schema = RESET`)
	gooseMu.Lock()
	err := goose.UpTo(db, "migrations", 20261110143421861)
	gooseMu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("unrecognized constraint was migrated: %v", err)
	}
}
