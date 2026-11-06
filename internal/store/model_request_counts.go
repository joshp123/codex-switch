package store

import "time"

type ModelRequestCounts struct {
	AccountKey  AccountKey
	Requests    int64
	Successful  int64
	Failed      int64
	Interrupted int64
}

type ModelRequestOutcome int

const (
	ModelRequestStarted ModelRequestOutcome = iota
	ModelRequestSuccessful
	ModelRequestFailed
	ModelRequestInterrupted
)

func (database *StoreDatabase) StartModelRequestObservation(now time.Time) error {
	_, err := database.sqlDatabase.Exec(`INSERT OR IGNORE INTO model_request_observation (singleton, started_at_unix_nano) VALUES (1, ?)`, now.UnixNano())
	return err
}

func (database *StoreDatabase) RecordModelRequestOutcome(account AccountKey, admittedAt time.Time, outcome ModelRequestOutcome) error {
	folder, err := database.accountFolder(account)
	if err != nil {
		return err
	}
	day := admittedAt.UTC().Format("2006-01-02")
	var requests, successful, failed, interrupted int
	switch outcome {
	case ModelRequestStarted:
		requests = 1
	case ModelRequestSuccessful:
		successful = 1
	case ModelRequestFailed:
		failed = 1
	case ModelRequestInterrupted:
		interrupted = 1
	}
	_, err = database.sqlDatabase.Exec(`INSERT INTO model_request_counts (account_name, account_key, admission_date, requests, successful, failed, interrupted)
	VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(account_name, admission_date) DO UPDATE SET account_key = excluded.account_key,
	requests = requests + excluded.requests, successful = successful + excluded.successful,
	failed = failed + excluded.failed, interrupted = interrupted + excluded.interrupted`, folder, account, day, requests, successful, failed, interrupted)
	return err
}

func (database *StoreDatabase) ReadModelRequestCounts() (time.Time, []ModelRequestCounts, error) {
	var startedAt int64
	err := database.sqlDatabase.QueryRow(`SELECT COALESCE(MAX(started_at_unix_nano), 0) FROM model_request_observation`).Scan(&startedAt)
	if err != nil {
		return time.Time{}, nil, err
	}
	if startedAt == 0 {
		return time.Time{}, nil, nil
	}
	accountKey := StoredAccountKey("model_request_counts")
	rows, err := database.sqlDatabase.Query(`SELECT ` + accountKey + `, SUM(requests), SUM(successful), SUM(failed), SUM(interrupted) FROM model_request_counts GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return time.Time{}, nil, err
	}
	defer rows.Close()
	var counts []ModelRequestCounts
	for rows.Next() {
		var count ModelRequestCounts
		if err := rows.Scan(&count.AccountKey, &count.Requests, &count.Successful, &count.Failed, &count.Interrupted); err != nil {
			return time.Time{}, nil, err
		}
		counts = append(counts, count)
	}
	return time.Unix(0, startedAt).UTC(), counts, rows.Err()
}
