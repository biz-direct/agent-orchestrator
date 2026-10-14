package sqlite

import (
	"strings"
	"testing"
)

func TestMigrateForkAndUpstreamVersionCollisions(t *testing.T) {
	for _, tc := range []struct {
		name            string
		base            int64
		upstream        int
		requestedOutput bool
	}{
		{name: "early install", base: 1},
		{name: "fork pipelines", base: 178},
		{name: "fork global rules", base: 179},
		{name: "fork archive", base: 195},
		{name: "fork sender", base: 196},
		{name: "upstream archive", base: 177, upstream: 1},
		{name: "upstream sender", base: 177, upstream: 2},
		{name: "upstream requested output", base: 177, upstream: 2, requestedOutput: true},
		{name: "requested output without archive", base: 194, requestedOutput: true},
		{name: "both physical schemas", base: 179, upstream: 2},
		{name: "merged requested output", base: 197},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openMigratedDatabaseCopy(t, tc.base)
			if tc.base == 179 {
				if _, err := db.Exec(`UPDATE app_settings SET global_orchestrator_rules = 'Keep my rules' WHERE id = 1`); err != nil {
					t.Fatal(err)
				}
			}
			for i, name := range []string{"0195_archive_reviewers.sql", "0196_conversation_message_sender.sql"} {
				if i >= tc.upstream {
					break
				}
				body, err := migrationsFS.ReadFile("migrations/" + name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(strings.Split(string(body), "-- +goose Down")[0]); err != nil {
					t.Fatal(err)
				}
				if tc.base == 177 {
					if _, err := db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, 178+i); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.requestedOutput {
				body, err := migrationsFS.ReadFile("migrations/0197_session_requested_output.sql")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(strings.Split(string(body), "-- +goose Down")[0]); err != nil {
					t.Fatal(err)
				}
				// Upstream shipped this column under 0195, not the fork's 0197.
				if _, err := db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (195, 1)`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`
INSERT INTO sessions (id, num, kind, activity_last_at, created_at, updated_at, requested_output)
VALUES ('artifact-session', 1, 'worker', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'artifact')`); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := migrate(db); err != nil {
					t.Fatal(err)
				}
			}
			for _, version := range []int{178, 179, 195, 196, 197} {
				var count int
				if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id = ? AND is_applied = 1`, version).Scan(&count); err != nil || count != 1 {
					t.Fatalf("version %d: applied rows = %d, err = %v", version, count, err)
				}
			}
			for table, columns := range map[string][]string{
				"sessions":              {"attached_to_session_id", "attached_for_attempt_id", "requested_output"},
				"pipeline_runs":         {"id", "snapshot"},
				"app_settings":          {"global_orchestrator_rules"},
				"review":                {"is_archived"},
				"conversation_messages": {"sender_session_id", "sender_project_id", "sender_display_name"},
			} {
				for _, column := range columns {
					var count int
					if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil || count != 1 {
						t.Fatalf("%s.%s: count = %d, err = %v", table, column, count, err)
					}
				}
			}
			if tc.requestedOutput {
				var output string
				if err := db.QueryRow(`SELECT requested_output FROM sessions WHERE id = 'artifact-session'`).Scan(&output); err != nil || output != "artifact" {
					t.Fatalf("requested output = %q, err = %v", output, err)
				}
			}
			if tc.base == 179 {
				var rules string
				if err := db.QueryRow(`SELECT global_orchestrator_rules FROM app_settings WHERE id = 1`).Scan(&rules); err != nil || rules != "Keep my rules" {
					t.Fatalf("rules = %q, err = %v", rules, err)
				}
			}
		})
	}
}

func TestChatHistoryRepairPreservesForkGlobalRulesVersion(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 179)
	// Hibernation can exist before sender metadata. It must not make the
	// legitimate fork 0179 marker look like an old hibernation preview marker.
	body, err := migrationsFS.ReadFile("migrations/0190_session_hibernation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(strings.Split(string(body), "-- +goose Down")[0]); err != nil {
		t.Fatal(err)
	}
	if err := repairRenumberedChatMigrationHistory(db); err != nil {
		t.Fatal(err)
	}
	var kept, moved int
	if err := db.QueryRow(`SELECT
    (SELECT COUNT(*) FROM goose_db_version WHERE version_id = 179 AND is_applied = 1),
    (SELECT COUNT(*) FROM goose_db_version WHERE version_id = 190 AND is_applied = 1)`).Scan(&kept, &moved); err != nil {
		t.Fatal(err)
	}
	if kept != 1 || moved != 0 {
		t.Fatalf("fork 179 rows = %d, hibernation 190 rows = %d", kept, moved)
	}
}

// Upstream shipped artifact versioning as 0196, which the fork uses for the
// conversation sender columns. A database that ran upstream's 0196 keeps its
// table, records the fork's 0251, and still gets the sender columns.
func TestMigrateUpstreamArtifactVersionsMovesTo0251(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 195)
	body, err := migrationsFS.ReadFile("migrations/0251_session_artifact_versions.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(strings.Split(string(body), "-- +goose Down")[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (196, 1)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	for _, version := range []int{196, 251} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id = ? AND is_applied = 1`, version).Scan(&count); err != nil || count != 1 {
			t.Fatalf("version %d: applied rows = %d, err = %v", version, count, err)
		}
	}
	var sender int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('conversation_messages') WHERE name = 'sender_session_id'`).Scan(&sender); err != nil || sender != 1 {
		t.Fatalf("sender column count = %d, err = %v", sender, err)
	}
}
