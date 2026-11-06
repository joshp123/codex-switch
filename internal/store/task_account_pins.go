package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReadTaskAccountPin reads current state on every turn so repairs take effect without restart.
func (database *StoreDatabase) ReadTaskAccountPin(taskID TaskID) (TaskAccountPin, bool, error) {
	var pin TaskAccountPin
	var pinnedAtUnixNano int64
	err := database.sqlDatabase.QueryRow(`
SELECT codex_task_id, account_name, pinned_at_unix_nano
FROM task_account_pins
WHERE codex_task_id = ?`, taskID).Scan(&pin.TaskID, &pin.AccountName, &pinnedAtUnixNano)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskAccountPin{}, false, nil
	}
	if err != nil {
		return TaskAccountPin{}, false, fmt.Errorf("read task account pin: %w", err)
	}
	pin.PinnedAt = time.Unix(0, pinnedAtUnixNano).UTC()
	return pin, true, nil
}

// PinTaskAccountIfAbsent returns the winner when two proxy generations first see the same task.
func (database *StoreDatabase) PinTaskAccountIfAbsent(candidate TaskAccountPin) (TaskAccountPin, error) {
	transaction, err := database.sqlDatabase.Begin()
	if err != nil {
		return TaskAccountPin{}, fmt.Errorf("begin task account pin: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(`
INSERT INTO task_account_pins (codex_task_id, account_name, pinned_at_unix_nano)
VALUES (?, ?, ?)
ON CONFLICT(codex_task_id) DO NOTHING`, candidate.TaskID, candidate.AccountName, candidate.PinnedAt.UTC().UnixNano()); err != nil {
		return TaskAccountPin{}, fmt.Errorf("insert task account pin: %w", err)
	}
	var winner TaskAccountPin
	var pinnedAtUnixNano int64
	if err := transaction.QueryRow(`
SELECT codex_task_id, account_name, pinned_at_unix_nano
FROM task_account_pins
WHERE codex_task_id = ?`, candidate.TaskID).Scan(&winner.TaskID, &winner.AccountName, &pinnedAtUnixNano); err != nil {
		return TaskAccountPin{}, fmt.Errorf("read inserted task account pin: %w", err)
	}
	winner.PinnedAt = time.Unix(0, pinnedAtUnixNano).UTC()
	if err := transaction.Commit(); err != nil {
		return TaskAccountPin{}, fmt.Errorf("commit task account pin: %w", err)
	}
	return winner, nil
}

// CompareAndSwapTaskAccountPin makes a concurrent exhaustion report a no-op after another process moves the pin.
func (database *StoreDatabase) CompareAndSwapTaskAccountPin(taskID TaskID, servedAccountName AccountName, nextAccountName AccountName, movedAt time.Time) (bool, error) {
	result, err := database.sqlDatabase.Exec(`
UPDATE task_account_pins
SET account_name = ?, pinned_at_unix_nano = ?
WHERE codex_task_id = ? AND account_name = ?`,
		nextAccountName, movedAt.UTC().UnixNano(), taskID, servedAccountName)
	if err != nil {
		return false, fmt.Errorf("move exhausted task pin: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read exhausted task pin move: %w", err)
	}
	return rowsAffected == 1, nil
}
