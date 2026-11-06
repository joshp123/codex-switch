package accounts

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

// releasedAccountTables are the account tables as the release before account
// keys created them. That release knows an account only by its folder name.
const releasedAccountTables = `
CREATE TABLE model_request_counts (account_name TEXT NOT NULL, admission_date TEXT NOT NULL, requests INTEGER NOT NULL,
    successful INTEGER NOT NULL, failed INTEGER NOT NULL, interrupted INTEGER NOT NULL, PRIMARY KEY (account_name, admission_date)) STRICT;
CREATE TABLE account_identities (account_name TEXT PRIMARY KEY, provider_account_id TEXT NOT NULL, email TEXT NOT NULL) STRICT;
CREATE TABLE task_account_pins (codex_task_id TEXT PRIMARY KEY, account_name TEXT NOT NULL, pinned_at_unix_nano INTEGER NOT NULL) STRICT;
CREATE TABLE account_reported_resets (account_name TEXT PRIMARY KEY, resets_at_unix_nano INTEGER NOT NULL, observed_at_unix_nano INTEGER NOT NULL) STRICT;
CREATE TABLE completed_responses (completed_response_id TEXT PRIMARY KEY, codex_task_id TEXT NOT NULL, account_name TEXT NOT NULL,
    model_slug TEXT NOT NULL, harness_mode TEXT NOT NULL, input_tokens INTEGER NOT NULL, cached_input_tokens INTEGER NOT NULL,
    cache_write_input_tokens INTEGER, output_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL,
    completed_at_unix_nano INTEGER NOT NULL, cost_nano_usd INTEGER) STRICT;
CREATE TABLE weekly_allowance_readings (account_name TEXT NOT NULL, resets_at_unix INTEGER NOT NULL, used_percent REAL NOT NULL,
    observed_at_unix_nano INTEGER NOT NULL, PRIMARY KEY (account_name, observed_at_unix_nano)) STRICT;
`

// A data directory written by the release before account keys converts once:
// every record takes its account's key, and keeps its folder name for that
// release, which may still be serving or be rolled back to.
func TestRecordAccountKeysConvertsRecordsWrittenByFolderName(t *testing.T) {
	data := t.TempDir()
	writeAccountFixture(t, filepath.Join(data, "account-1"), "user-one", "One@Example.com", "access-one")
	writeClaudeAccountFixture(t, filepath.Join(data, "account-4"), "one@example.com")
	databasePath := filepath.Join(data, "codex-switch.db")
	released, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	// account-2 was removed; account-4 signed in before identities were
	// recorded; account-9 has no recorded identity at all.
	for _, statement := range []string{
		releasedAccountTables,
		`INSERT INTO account_identities VALUES ('account-1', 'subscription-user-one', 'One@Example.com'), ('account-2', 'subscription-user-two', 'two@example.com')`,
		`INSERT INTO task_account_pins VALUES ('codex-task', 'account-1', 1), ('claude-task', 'account-4', 2)`,
		`INSERT INTO account_reported_resets VALUES ('account-1', 3, 4)`,
		`INSERT INTO model_request_counts VALUES ('account-1', '2026-11-06', 5, 4, 1, 0)`,
		`INSERT INTO completed_responses VALUES
		    ('response-1', 'codex-task', 'account-1', 'gpt-5.6', 'standard', 10, 0, NULL, 5, 0, 1, NULL),
		    ('response-2', 'old-task', 'account-2', 'gpt-5.6', 'standard', 10, 0, NULL, 5, 0, 2, NULL),
		    ('response-4', 'claude-task', 'account-4', 'claude-opus-5-5', 'standard', 10, 0, 0, 5, 0, 3, NULL),
		    ('response-9', 'older-task', 'account-9', 'gpt-5.5', 'standard', 10, 0, NULL, 5, 0, 4, NULL)`,
		`INSERT INTO weekly_allowance_readings VALUES ('account-4', 100, 20, 5)`,
	} {
		if _, err := released.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	released.Close()

	database, err := store.OpenStoreDatabase(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.CloseStoreDatabase()
	filled, err := RecordAccountKeys(data, database)
	if err != nil {
		t.Fatal(err)
	}
	if filled != 8 {
		t.Fatalf("filled %d records, want every record of a known account: 8", filled)
	}
	want := map[string]string{
		"account-1": "chatgpt:one@example.com", "account-2": "chatgpt:two@example.com",
		"account-4": "claude:one@example.com", "account-9": "",
	}
	for _, table := range []string{"account_identities", "task_account_pins", "account_reported_resets", "model_request_counts", "completed_responses", "weekly_allowance_readings"} {
		rows, err := database.SQLDatabase().Query(`SELECT account_name, COALESCE(account_key, '') FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var folder, key string
			if err := rows.Scan(&folder, &key); err != nil {
				t.Fatal(err)
			}
			if key != want[folder] {
				t.Errorf("%s: %s has key %q, want %q", table, folder, key, want[folder])
			}
		}
		rows.Close()
	}

	// The released proxy may still pin a task by folder name while it drains.
	if _, err := database.SQLDatabase().Exec(`INSERT INTO task_account_pins (codex_task_id, account_name, pinned_at_unix_nano) VALUES ('draining-task', 'account-4', 6)`); err != nil {
		t.Fatal(err)
	}
	pin, pinned, err := database.ReadTaskAccountPin("draining-task")
	if err != nil || !pinned || pin.AccountKey != "claude:one@example.com" {
		t.Fatalf("pin written by folder name = %+v, %v, %v", pin, pinned, err)
	}
	moved, err := database.CompareAndSwapTaskAccountPin("draining-task", "claude:one@example.com", "chatgpt:one@example.com", time.Unix(7, 0))
	if err != nil || !moved {
		t.Fatalf("moving a pin written by folder name: %v, %v", moved, err)
	}
	var folder string
	if err := database.SQLDatabase().QueryRow(`SELECT account_name FROM task_account_pins WHERE codex_task_id = 'draining-task'`).Scan(&folder); err != nil || folder != "account-1" {
		t.Fatalf("moved pin keeps folder %q for the released proxy: %v", folder, err)
	}
	// The released proxy moves an exhausted task's pin by folder name alone,
	// leaving the key the pin had; the folder decides.
	if _, err := database.SQLDatabase().Exec(`UPDATE task_account_pins SET account_name = 'account-4', pinned_at_unix_nano = 8
WHERE codex_task_id = 'draining-task' AND account_name = 'account-1'`); err != nil {
		t.Fatal(err)
	}
	if pin, _, err := database.ReadTaskAccountPin("draining-task"); err != nil || pin.AccountKey != "claude:one@example.com" {
		t.Fatalf("pin the released proxy moved = %+v, %v", pin, err)
	}
	if synced, err := RecordAccountKeys(data, database); err != nil || synced != 1 {
		t.Fatalf("syncing the moved pin's key changed %d records: %v", synced, err)
	}
	var key string
	if err := database.SQLDatabase().QueryRow(`SELECT account_key FROM task_account_pins WHERE codex_task_id = 'draining-task'`).Scan(&key); err != nil || key != "claude:one@example.com" {
		t.Fatalf("moved pin stores key %q: %v", key, err)
	}
	if _, found, err := database.ReadAccountReportedReset("chatgpt:one@example.com"); err != nil || !found {
		t.Fatalf("reported reset: %v, %v", found, err)
	}
	if err := database.StartModelRequestObservation(time.Unix(8, 0)); err != nil {
		t.Fatal(err)
	}
	_, counts, err := database.ReadModelRequestCounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 1 || counts[0].AccountKey != "chatgpt:one@example.com" || counts[0].Requests != 5 {
		t.Fatalf("request counts = %+v", counts)
	}
}

func writeClaudeAccountFixture(t *testing.T, home, email string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(claudeAuthFile{
		Provider: ProviderClaude, AccessToken: "claude-access", RefreshToken: "claude-refresh",
		ExpiresAt: time.Now().Add(time.Hour), AccountID: "claude-account", OrganizationID: "claude-organization",
		Email: email, Plan: "Max 20x",
	})
	if err := os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
