package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

// usageQuery lets all fold inputs share the same SQLite read snapshot.
type usageQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type usageRowWatermarks struct {
	CompletedResponses int64
	NativeObservations int64
	NativeSources      int64
	NativeChildren     int64
	MutationGeneration int64
}

func readUsageRowWatermarks(ctx context.Context, query usageQuery) (usageRowWatermarks, error) {
	var marks usageRowWatermarks
	err := query.QueryRowContext(ctx, `SELECT
 (SELECT COALESCE(MAX(rowid),0) FROM completed_responses),
 (SELECT COALESCE(MAX(rowid),0) FROM native_usage_observations),
 (SELECT COALESCE(MAX(rowid),0) FROM native_usage_sources),
 (SELECT COALESCE(MAX(rowid),0) FROM native_task_child_records),
 (SELECT generation FROM usage_mutation_counter WHERE singleton=1)`).Scan(&marks.CompletedResponses, &marks.NativeObservations, &marks.NativeSources, &marks.NativeChildren, &marks.MutationGeneration)
	return marks, err
}

var nativeObservationColumns = []string{"task_id", "source_offset", "response_id", "root_turn_id", "session_id", "turn_id", "observed", "model", "tier", "input", "cached", "cache_write", "output", "reasoning", "total", "limit_id", "plan_type", "primary_used", "primary_minutes", "primary_resets", "secondary_used", "secondary_minutes", "secondary_resets"}

// Duplicate imports must preserve rowids. Conflicting bytes at an already
// imported offset indicate a rewrite and must use the reset path instead.
func insertIdenticalNativeRow(ctx context.Context, transaction *sql.Tx, table string, columns []string, arguments []any, keyColumns int) error {
	placeholders := make([]string, len(columns))
	for index := range placeholders {
		placeholders[index] = "?"
	}
	result, err := transaction.ExecContext(ctx, "INSERT INTO "+table+" VALUES ("+strings.Join(placeholders, ",")+") ON CONFLICT DO NOTHING", arguments...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 0 {
		return err
	}
	comparisons := make([]string, len(columns))
	for index, column := range columns {
		comparisons[index] = column + " IS ?"
	}
	keys := comparisons[:keyColumns]
	var identical bool
	queryArguments := append(append([]any{}, arguments...), arguments[:keyColumns]...)
	err = transaction.QueryRowContext(ctx, "SELECT "+strings.Join(comparisons, " AND ")+" FROM "+table+" WHERE "+strings.Join(keys, " AND "), queryArguments...).Scan(&identical)
	if err != nil {
		return err
	}
	if !identical {
		return fmt.Errorf("conflicting native import in %s at an existing source offset", table)
	}
	return nil
}

func streamNativeSources(ctx context.Context, query usageQuery, afterRowID, throughRowID int64, visit func(int64, taskMetadata) error) error {
	rows, err := query.QueryContext(ctx, `SELECT rowid,task_id,path,parent_task_id,forked_from_task_id,created,source_kind FROM native_usage_sources WHERE rowid>? AND rowid<=? ORDER BY rowid`, afterRowID, throughRowID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rowID, created int64
		var metadata taskMetadata
		if err = rows.Scan(&rowID, &metadata.TaskID, &metadata.SourcePath, &metadata.ParentTaskID, &metadata.ForkedFromTaskID, &created, &metadata.SourceKind); err != nil {
			return err
		}
		metadata.CreatedAt = time.Unix(0, created)
		if err = visit(rowID, metadata); err != nil {
			return err
		}
	}
	return rows.Err()
}

func streamNativeChildren(ctx context.Context, query usageQuery, afterRowID, throughRowID int64, visit func(int64, string, string) error) error {
	rows, err := query.QueryContext(ctx, `SELECT rowid,source_task_id,child_task_id FROM native_task_child_records WHERE rowid>? AND rowid<=? ORDER BY rowid`, afterRowID, throughRowID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rowID int64
		var parent, child string
		if err = rows.Scan(&rowID, &parent, &child); err != nil {
			return err
		}
		if err = visit(rowID, parent, child); err != nil {
			return err
		}
	}
	return rows.Err()
}

func scanSessionUsageWithResponse(rows *sql.Rows, rowID *int64) (sessionUsageObservation, *store.CompletedResponseRecord, error) {
	var proxy store.CompletedResponseRecord
	var proxyAt int64
	var observation sessionUsageObservation
	var observed int64
	var input, cached, cacheWrite, output, reasoning, total, primaryMinutes, primaryResets, secondaryMinutes, secondaryResets sql.NullInt64
	var limitID, planType sql.NullString
	var primaryUsed, secondaryUsed sql.NullFloat64
	var destinations [39]any
	fields := destinations[:0]
	fields = append(fields, rowID)
	fields = append(fields, &observation.TaskID, &observation.SourcePath, &observation.SourceKind, &observation.SourceOffset, &observation.ResponseID, &observation.RootTurnID, &observation.SessionID, &observation.TurnID, &observed, &observation.Model, &observation.ServiceTier, &input, &cached, &cacheWrite, &output, &reasoning, &total, &limitID, &planType, &primaryUsed, &primaryMinutes, &primaryResets, &secondaryUsed, &secondaryMinutes, &secondaryResets)
	fields = append(fields, &proxy.CompletedResponseID, &proxy.TaskID, &proxy.AccountKey, &proxy.ModelSlug, &proxy.HarnessMode, &proxy.InputTokens, &proxy.CachedInputTokens, &proxy.CacheWriteInputTokens, &proxy.OutputTokens, &proxy.ReasoningTokens, &proxyAt, &proxy.CostNanoUSD)
	if err := rows.Scan(fields...); err != nil {
		return observation, nil, err
	}
	observation.At = time.Unix(0, observed)
	if input.Valid {
		observation.Usage = &tokenUsage{InputTokens: input.Int64, CachedInputTokens: cached.Int64, OutputTokens: output.Int64, ReasoningOutputTokens: reasoning.Int64, TotalTokens: total.Int64}
		if cacheWrite.Valid {
			observation.Usage.CacheWriteInputTokens = &cacheWrite.Int64
		}
	}
	if limitID.Valid {
		observation.Allowance = &sessionAllowance{LimitID: limitID.String, PlanType: planType.String}
		if primaryUsed.Valid {
			observation.Allowance.Primary = &sessionAllowanceWindow{UsedPercent: primaryUsed.Float64, WindowMinutes: primaryMinutes.Int64, ResetsAt: time.Unix(0, primaryResets.Int64)}
		}
		if secondaryUsed.Valid {
			observation.Allowance.Secondary = &sessionAllowanceWindow{UsedPercent: secondaryUsed.Float64, WindowMinutes: secondaryMinutes.Int64, ResetsAt: time.Unix(0, secondaryResets.Int64)}
		}
	}
	if proxy.CompletedResponseID != "" {
		proxy.CompletedAt = time.Unix(0, proxyAt)
		return observation, &proxy, nil
	}
	return observation, nil, nil
}

func streamNativeUsageWithResponses(ctx context.Context, query usageQuery, afterRowID, throughRowID int64, start, end time.Time, usageOnly bool, visit func(int64, sessionUsageObservation, *store.CompletedResponseRecord) error) error {
	rows, err := query.QueryContext(ctx, `SELECT o.rowid,o.task_id,s.path,s.source_kind,o.source_offset,o.response_id,o.root_turn_id,o.session_id,o.turn_id,o.observed,o.model,o.tier,o.input,o.cached,o.cache_write,o.output,o.reasoning,o.total,o.limit_id,o.plan_type,o.primary_used,o.primary_minutes,o.primary_resets,o.secondary_used,o.secondary_minutes,o.secondary_resets,
 COALESCE(p.completed_response_id,''),COALESCE(p.codex_task_id,''),COALESCE(`+store.StoredAccountKey("p")+`,''),COALESCE(p.model_slug,''),COALESCE(p.harness_mode,''),COALESCE(p.input_tokens,0),COALESCE(p.cached_input_tokens,0),p.cache_write_input_tokens,COALESCE(p.output_tokens,0),COALESCE(p.reasoning_tokens,0),COALESCE(p.completed_at_unix_nano,0),p.cost_nano_usd
 FROM native_usage_observations o NOT INDEXED JOIN native_usage_sources s ON o.task_id=s.task_id LEFT JOIN completed_responses p ON p.completed_response_id=o.response_id AND p.completed_at_unix_nano>=? WHERE o.rowid>? AND o.rowid<=? AND o.observed>=? AND o.observed<? AND (?=0 OR o.input IS NOT NULL) ORDER BY o.observed,o.task_id,o.source_offset`, start.UnixNano(), afterRowID, throughRowID, start.UnixNano(), end.UnixNano(), usageOnly)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rowID int64
		observation, proxy, err := scanSessionUsageWithResponse(rows, &rowID)
		if err != nil {
			return err
		}
		if err = visit(rowID, observation, proxy); err != nil {
			return err
		}
	}
	return rows.Err()
}
