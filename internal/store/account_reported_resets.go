package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RecordAccountReportedReset preserves account-wide provider exhaustion independently of any task pin.
func (database *StoreDatabase) RecordAccountReportedReset(accountKey AccountKey, resetsAt, observedAt time.Time) error {
	folder, err := database.accountFolder(accountKey)
	if err != nil {
		return err
	}
	var resetsAtUnixNano int64
	if !resetsAt.IsZero() {
		resetsAtUnixNano = resetsAt.UTC().UnixNano()
	}
	_, err = database.sqlDatabase.Exec(`
INSERT INTO account_reported_resets (account_name, account_key, resets_at_unix_nano, observed_at_unix_nano)
VALUES (?, ?, ?, ?)
ON CONFLICT(account_name) DO UPDATE SET account_key = excluded.account_key,
    resets_at_unix_nano = excluded.resets_at_unix_nano, observed_at_unix_nano = excluded.observed_at_unix_nano
WHERE excluded.observed_at_unix_nano >= account_reported_resets.observed_at_unix_nano`,
		folder, accountKey, resetsAtUnixNano, observedAt.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("record account reported reset: %w", err)
	}
	return nil
}

// ReadAccountReportedReset adjusts provider snapshots with later exhaustion evidence.
func (database *StoreDatabase) ReadAccountReportedReset(accountKey AccountKey) (time.Time, bool, error) {
	var resetsAtUnixNano int64
	err := database.sqlDatabase.QueryRow(`
SELECT resets_at_unix_nano
FROM account_reported_resets
WHERE `+StoredAccountKey("account_reported_resets")+` = ?
ORDER BY observed_at_unix_nano DESC LIMIT 1`, accountKey).Scan(&resetsAtUnixNano)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read account reported reset: %w", err)
	}
	if resetsAtUnixNano == 0 {
		return time.Time{}, true, nil
	}
	return time.Unix(0, resetsAtUnixNano).UTC(), true, nil
}

// ClearAccountExhaustionBefore lets a successful later provider read restore capacity.
// It reports whether it cleared a recorded exhaustion.
func (database *StoreDatabase) ClearAccountExhaustionBefore(accountKey AccountKey, observedAt time.Time) (bool, error) {
	result, err := database.sqlDatabase.Exec(`DELETE FROM account_reported_resets
WHERE `+StoredAccountKey("account_reported_resets")+` = ? AND observed_at_unix_nano < ?`, accountKey, observedAt.UTC().UnixNano())
	if err != nil {
		return false, fmt.Errorf("clear restored account exhaustion: %w", err)
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read restored account exhaustion: %w", err)
	}
	return cleared > 0, nil
}
