package codexstate

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
)

// ThreadRollout identifies a rollout changed in Codex's own thread index.
// Usage and lineage still come from the rollout, never the index.
type ThreadRollout struct {
	ID          string
	Path        string
	UpdatedAtMS int64
	Archived    bool
}

func (r LocalReader) UpdatedThreadRollouts(ctx context.Context, sinceMS int64) (string, []ThreadRollout, error) {
	path, err := r.taskTitleDatabase()
	if err != nil {
		return "", nil, err
	}
	if path == "" {
		return "", nil, fmt.Errorf("Codex thread index is absent")
	}
	databaseURL := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	database, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return path, nil, err
	}
	defer database.Close()
	rows, err := database.QueryContext(ctx, `SELECT id,rollout_path,updated_at_ms,archived FROM threads WHERE updated_at_ms>=? ORDER BY updated_at_ms`, sinceMS)
	if err != nil {
		return path, nil, fmt.Errorf("read Codex thread index: %w", err)
	}
	defer rows.Close()
	var result []ThreadRollout
	for rows.Next() {
		var thread ThreadRollout
		if err := rows.Scan(&thread.ID, &thread.Path, &thread.UpdatedAtMS, &thread.Archived); err != nil {
			return path, nil, fmt.Errorf("read Codex thread index: %w", err)
		}
		if thread.ID == "" || thread.Path == "" {
			return path, nil, fmt.Errorf("Codex thread index has an empty thread ID or rollout path")
		}
		result = append(result, thread)
	}
	return path, result, rows.Err()
}
