package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AccountIdentity records which folder in the data directory holds which
// account. It outlives the account's removal from the pool, so records written
// with only the folder name still find their account key.
type AccountIdentity struct {
	AccountFolder     string
	AccountKey        AccountKey
	ProviderAccountID string
	Email             string
}

func (database *StoreDatabase) ReadAccountIdentities() ([]AccountIdentity, error) {
	rows, err := database.sqlDatabase.Query(`SELECT account_name, COALESCE(account_key, ''), provider_account_id, email FROM account_identities ORDER BY account_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var identities []AccountIdentity
	for rows.Next() {
		var identity AccountIdentity
		if err := rows.Scan(&identity.AccountFolder, &identity.AccountKey, &identity.ProviderAccountID, &identity.Email); err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

func (database *StoreDatabase) RememberAccountIdentity(identity AccountIdentity) error {
	if identity.AccountKey == "" {
		return fmt.Errorf("account identity for %s has no account key", identity.AccountFolder)
	}
	result, err := database.sqlDatabase.Exec(`
INSERT INTO account_identities (account_name, provider_account_id, email, account_key)
VALUES (?, ?, ?, ?)
ON CONFLICT(account_name) DO UPDATE SET email = excluded.email, account_key = excluded.account_key
WHERE account_identities.provider_account_id = excluded.provider_account_id`,
		identity.AccountFolder, identity.ProviderAccountID, identity.Email, identity.AccountKey)
	if err != nil {
		return fmt.Errorf("remember account identity: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("account folder %s already belongs to another account", identity.AccountFolder)
	}
	return nil
}

// StoredAccountKey is the SQL for the account key of a row of table: the key
// recorded for the row's folder. The folder decides, because builds before
// account keys know an account only by its folder and may still write: while
// an update switches proxies, or after deploy.sh back. Such a build moves a
// task's pin by changing its folder and leaves the row's key as it was. The
// row's own key counts only where its folder has no recorded key, as in a row
// copied from another Mac; a row with neither keeps its folder name, which
// matches no signed-in account.
func StoredAccountKey(table string) string {
	return `COALESCE((SELECT account_identities.account_key FROM account_identities WHERE account_identities.account_name = ` + table + `.account_name), ` + table + `.account_key, ` + table + `.account_name)`
}

// accountFolder is the folder that holds the account now. Folder numbers are
// never reused, so an account removed and signed in again is in its
// highest-numbered folder.
func (database *StoreDatabase) accountFolder(key AccountKey) (string, error) {
	var folder string
	err := database.sqlDatabase.QueryRow(`
SELECT account_name FROM account_identities WHERE account_key = ?
ORDER BY CAST(SUBSTR(account_name, LENGTH('account-') + 1) AS INTEGER) DESC LIMIT 1`, key).Scan(&folder)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no folder is recorded for account %s", key)
	}
	if err != nil {
		return "", fmt.Errorf("read the folder of account %s: %w", key, err)
	}
	return folder, nil
}

// accountKeyTables are the tables with a row per account or per account event.
var accountKeyTables = []string{"task_account_pins", "account_reported_resets", "completed_responses", "model_request_counts", "weekly_allowance_readings"}

// SyncAccountKeys sets the account key on each row to the key recorded for its
// folder, so the column agrees with StoredAccountKey for anyone who reads the
// table directly or combines it with another Mac's. A build before account keys
// writes rows without a key, and moves pins without changing theirs. It
// returns how many rows it changed.
func (database *StoreDatabase) SyncAccountKeys() (int64, error) {
	transaction, err := database.sqlDatabase.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin syncing account keys: %w", err)
	}
	defer transaction.Rollback()
	var changed int64
	for _, table := range accountKeyTables {
		folderKey := `(SELECT account_identities.account_key FROM account_identities WHERE account_identities.account_name = ` + table + `.account_name)`
		result, err := transaction.Exec(`UPDATE ` + table + ` SET account_key = ` + folderKey + `
WHERE account_key IS NOT ` + folderKey + ` AND ` + folderKey + ` IS NOT NULL`)
		if err != nil {
			return 0, fmt.Errorf("sync account keys in %s: %w", table, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		changed += rows
	}
	if err := transaction.Commit(); err != nil {
		return 0, fmt.Errorf("commit account keys: %w", err)
	}
	return changed, nil
}

// NextAccountNumber is called under the pool mutation lock, after remembering existing accounts.
func (database *StoreDatabase) NextAccountNumber() (int, error) {
	rows, err := database.sqlDatabase.Query(`
SELECT account_name FROM account_identities
UNION SELECT account_name FROM completed_responses
UNION SELECT account_name FROM task_account_pins`)
	if err != nil {
		return 0, fmt.Errorf("read allocated account folders: %w", err)
	}
	defer rows.Close()
	next := 1
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, err
		}
		number, err := strconv.Atoi(strings.TrimPrefix(name, "account-"))
		if err != nil {
			return 0, fmt.Errorf("invalid stored account folder %q", name)
		}
		if number >= next {
			next = number + 1
		}
	}
	return next, rows.Err()
}
