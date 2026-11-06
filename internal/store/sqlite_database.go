package store

import (
	"database/sql"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// StoreDatabase is shared by old and new proxy generations during a live drain.
type StoreDatabase struct {
	sqlDatabase              *sql.DB
	weeklyAllowanceBurnMutex sync.Mutex
	weeklyAllowanceBurnRowID int64
	weeklyAllowanceBurns     []weeklyAllowanceBurn
}

// OpenStoreDatabase enables WAL before exposing the database to concurrent proxy generations.
func OpenStoreDatabase(databasePath string) (*StoreDatabase, error) {
	sqlDatabase, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, fmt.Errorf("open store database: %w", err)
	}
	// One long-lived connection makes connection-scoped SQLite pragmas apply to every operation.
	sqlDatabase.SetMaxOpenConns(1)
	if _, err := sqlDatabase.Exec(`PRAGMA busy_timeout = 5000; PRAGMA journal_mode = WAL;`); err != nil {
		sqlDatabase.Close()
		return nil, fmt.Errorf("configure store database: %w", err)
	}
	if _, err := sqlDatabase.Exec(storeDatabaseSchema); err != nil {
		sqlDatabase.Close()
		return nil, fmt.Errorf("create store database schema: %w", err)
	}
	if err := createAllowanceWindowIdentitySchema(sqlDatabase); err != nil {
		sqlDatabase.Close()
		return nil, fmt.Errorf("create allowance window identity schema: %w", err)
	}
	if err := createWeeklyAllowanceSchema(sqlDatabase); err != nil {
		sqlDatabase.Close()
		return nil, fmt.Errorf("create weekly allowance schema: %w", err)
	}
	if err := addColumns(sqlDatabase); err != nil {
		sqlDatabase.Close()
		return nil, fmt.Errorf("add store database columns: %w", err)
	}
	return &StoreDatabase{sqlDatabase: sqlDatabase}, nil
}

// addedColumns are columns added to tables that earlier builds created.
// Earlier builds keep working on the database: they name every column they
// write, and leave a new column empty.
var addedColumns = []struct{ table, column, columnType string }{
	{"completed_responses", "cache_write_input_tokens", "INTEGER"},
	{"completed_responses", "cost_nano_usd", "INTEGER"},
	{"account_identities", "account_key", "TEXT"},
	{"task_account_pins", "account_key", "TEXT"},
	{"account_reported_resets", "account_key", "TEXT"},
	{"completed_responses", "account_key", "TEXT"},
	{"model_request_counts", "account_key", "TEXT"},
	{"weekly_allowance_readings", "account_key", "TEXT"},
}

func addColumns(sqlDatabase *sql.DB) error {
	// Serialize the check and addition when proxy generations start together.
	if _, err := sqlDatabase.Exec(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer sqlDatabase.Exec(`ROLLBACK`)
	for _, added := range addedColumns {
		var columnExists bool
		if err := sqlDatabase.QueryRow(`SELECT EXISTS (SELECT 1 FROM pragma_table_info(?) WHERE name = ?)`, added.table, added.column).Scan(&columnExists); err != nil {
			return err
		}
		if !columnExists {
			if _, err := sqlDatabase.Exec(`ALTER TABLE ` + added.table + ` ADD COLUMN ` + added.column + ` ` + added.columnType); err != nil {
				return err
			}
		}
	}
	_, err := sqlDatabase.Exec(`COMMIT`)
	return err
}

// SQLDatabase shares this process's one connection with a module that owns its
// own tables, so its writes queue behind the same connection as routing writes.
func (database *StoreDatabase) SQLDatabase() *sql.DB {
	return database.sqlDatabase
}

// CloseStoreDatabase releases only this process's connection and never affects another generation.
func (database *StoreDatabase) CloseStoreDatabase() error {
	return database.sqlDatabase.Close()
}

// Tables that record an account store its key in account_key and the name of
// its folder in the data directory, such as account-3, in account_name. Builds
// before account keys know an account only by its folder; they may run on this
// database while an update switches proxies, or after a rollback, and they leave
// account_key empty. See FillAccountKeys.
const storeDatabaseSchema = `
CREATE TABLE IF NOT EXISTS prompt_cache_affinity (
    prefix_hash TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    seen_at_unix_nano INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS model_request_observation (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    started_at_unix_nano INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS model_request_counts (
    account_name TEXT NOT NULL,
    admission_date TEXT NOT NULL,
    requests INTEGER NOT NULL,
    successful INTEGER NOT NULL,
    failed INTEGER NOT NULL,
    interrupted INTEGER NOT NULL,
    account_key TEXT,
    PRIMARY KEY (account_name, admission_date)
) STRICT;

CREATE TABLE IF NOT EXISTS account_identities (
    account_name TEXT PRIMARY KEY,
    provider_account_id TEXT NOT NULL,
    email TEXT NOT NULL,
    account_key TEXT
) STRICT;

CREATE TABLE IF NOT EXISTS task_account_pins (
    codex_task_id TEXT PRIMARY KEY,
    account_name TEXT NOT NULL,
    pinned_at_unix_nano INTEGER NOT NULL,
    account_key TEXT
) STRICT;

CREATE TABLE IF NOT EXISTS account_reported_resets (
    account_name TEXT PRIMARY KEY,
    resets_at_unix_nano INTEGER NOT NULL,
    observed_at_unix_nano INTEGER NOT NULL,
    account_key TEXT
) STRICT;

CREATE TABLE IF NOT EXISTS completed_responses (
    completed_response_id TEXT PRIMARY KEY,
    codex_task_id TEXT NOT NULL,
    account_name TEXT NOT NULL,
    model_slug TEXT NOT NULL,
    harness_mode TEXT NOT NULL,
    input_tokens INTEGER NOT NULL,
    cached_input_tokens INTEGER NOT NULL,
    cache_write_input_tokens INTEGER,
    output_tokens INTEGER NOT NULL,
    reasoning_tokens INTEGER NOT NULL,
    completed_at_unix_nano INTEGER NOT NULL,
    cost_nano_usd INTEGER,
    account_key TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS completed_responses_completed_at
ON completed_responses(completed_at_unix_nano);
`
