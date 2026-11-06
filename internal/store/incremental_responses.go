package store

import (
	"context"
	"database/sql"
	"time"
)

// ResponseQuery permits callers to include responses in their own read snapshot.
type ResponseQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// StreamCompletedResponses reads each newly appended row without retaining
// historical responses. Bounds come from the caller's read transaction.
func StreamCompletedResponses(ctx context.Context, query ResponseQuery, afterRowID, throughRowID int64, start, end time.Time, visit func(int64, CompletedResponseRecord) error) error {
	rows, err := query.QueryContext(ctx, `SELECT rowid,completed_response_id,codex_task_id,`+StoredAccountKey("completed_responses")+`,model_slug,harness_mode,input_tokens,cached_input_tokens,cache_write_input_tokens,output_tokens,reasoning_tokens,completed_at_unix_nano,cost_nano_usd FROM completed_responses WHERE rowid>? AND rowid<=? AND completed_at_unix_nano>=? AND completed_at_unix_nano<? ORDER BY rowid`, afterRowID, throughRowID, start.UnixNano(), end.UnixNano())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rowID int64
		record, err := scanCompletedResponse(rows, &rowID)
		if err != nil {
			return err
		}
		if err = visit(rowID, record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func scanCompletedResponse(rows *sql.Rows, rowID *int64) (CompletedResponseRecord, error) {
	var record CompletedResponseRecord
	var completedAt int64
	var destinations [14]any
	fields := destinations[:0]
	fields = append(fields, rowID)
	fields = append(fields, &record.CompletedResponseID, &record.TaskID, &record.AccountKey, &record.ModelSlug, &record.HarnessMode, &record.InputTokens, &record.CachedInputTokens, &record.CacheWriteInputTokens, &record.OutputTokens, &record.ReasoningTokens, &completedAt, &record.CostNanoUSD)
	err := rows.Scan(fields...)
	record.CompletedAt = time.Unix(0, completedAt)
	return record, err
}
