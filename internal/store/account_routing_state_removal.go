package store

import "fmt"

// RemoveAccountRoutingState retires routing state while preserving account identity and completed usage.
func (database *StoreDatabase) RemoveAccountRoutingState(accountKey AccountKey) (int64, error) {
	transaction, err := database.sqlDatabase.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin account routing state removal: %w", err)
	}
	defer transaction.Rollback()
	pinResult, err := transaction.Exec(`DELETE FROM task_account_pins WHERE `+StoredAccountKey("task_account_pins")+` = ?`, accountKey)
	if err != nil {
		return 0, fmt.Errorf("remove account task pins: %w", err)
	}
	removedPinCount, err := pinResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read removed account task pin count: %w", err)
	}
	if _, err := transaction.Exec(`DELETE FROM account_reported_resets WHERE `+StoredAccountKey("account_reported_resets")+` = ?`, accountKey); err != nil {
		return 0, fmt.Errorf("remove account reported reset: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, fmt.Errorf("commit account routing state removal: %w", err)
	}
	return removedPinCount, nil
}
