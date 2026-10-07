package sqlite

import (
	"strings"
	"testing"
)

// The pipeline schema ships as one migration, so a crash during it leaves the
// previous version intact. A rebuild of an existing table under NO TRANSACTION
// (drop, rename) could lose rows or strand a half-built table.
func TestPipelineMigrationIsOneTransactionalMigration(t *testing.T) {
	raw, err := migrationsFS.ReadFile("migrations/0178_pipeline_runs.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "NO TRANSACTION") || strings.Contains(strings.ToUpper(string(raw)), "PRAGMA FOREIGN_KEYS") {
		t.Fatal("the pipeline migration must run inside a transaction without toggling foreign keys")
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "pipeline") && e.Name() != "0178_pipeline_runs.sql" {
			t.Fatalf("unexpected extra pipeline migration %s: the schema is carried by 0178", e.Name())
		}
	}
}

func TestPipelineMigrationDownRemovesEverythingAndUpRestoresIt(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 178)
	count := func(query string) int {
		var n int
		if err := db.QueryRow(query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	const pipelineObjects = `SELECT count(*) FROM sqlite_master WHERE name LIKE '%pipeline%'`
	const attachedColumns = `SELECT count(*) FROM pragma_table_info('sessions') WHERE name IN ('attached_to_session_id', 'attached_for_attempt_id')`
	if count(pipelineObjects) == 0 || count(attachedColumns) != 2 {
		t.Fatal("the pipeline schema must be present at 178")
	}
	downTo(t, db, 177)
	if n := count(pipelineObjects); n != 0 {
		t.Fatalf("down left %d pipeline objects behind", n)
	}
	if n := count(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'idx_sessions_attached%'`) + count(attachedColumns); n != 0 {
		t.Fatalf("down left the attached-session columns or indexes behind: %d", n)
	}
	upTo(t, db, 178)
	if count(pipelineObjects) == 0 || count(attachedColumns) != 2 {
		t.Fatal("up after down must restore the schema")
	}
}
