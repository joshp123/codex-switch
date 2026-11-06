package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

func TestNativeArrivalMovesPendingWorkWithoutLosingPreviousLatestTime(t *testing.T) {
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	older := now.Add(-30 * time.Second)
	proxyAt := now.Add(-20 * time.Second)
	nativeAt := now.Add(-21 * time.Second)
	insertFoldNative(t, ledger, sessionUsageObservation{TaskID: "original", ResponseID: "older", At: older, Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 10, OutputTokens: 2}})
	recordFoldProxy(t, ledger, "moving", "original", proxyAt, 20, 3)
	before := refreshTestFold(t, ledger, now)
	original := testFoldTasks(t, ledger, before)["original"]
	if original.Tokens != 35 || !original.LastCompletedAt.Equal(proxyAt) || len(before.pending) != 1 {
		t.Fatalf("pending work: tokens=%d latest=%s pending=%d", original.Tokens, original.LastCompletedAt, len(before.pending))
	}
	insertFoldNative(t, ledger, sessionUsageObservation{TaskID: "destination", ResponseID: "moving", At: nativeAt, Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 20, OutputTokens: 3}})
	after := refreshTestFold(t, ledger, now)
	if after != before {
		t.Fatal("common native arrival unnecessarily refolded")
	}
	tasks := testFoldTasks(t, ledger, after)
	if tasks["original"].Tokens != 12 || !tasks["original"].LastCompletedAt.Equal(older) {
		t.Fatalf("previous task lost its settled latest time: %+v", tasks["original"])
	}
	if tasks["destination"].Tokens != 23 || !tasks["destination"].LastCompletedAt.Equal(proxyAt) || len(after.pending) != 0 {
		t.Fatalf("reconciled task: %+v pending=%d", tasks["destination"], len(after.pending))
	}
}

func TestProxyAfterNativeRefoldsAndCountsResponseOnce(t *testing.T) {
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	insertFoldNative(t, ledger, sessionUsageObservation{TaskID: "task", ResponseID: "late-proxy", At: now.Add(-2 * time.Second), Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 10, OutputTokens: 2}})
	before := refreshTestFold(t, ledger, now)
	if foldTestCohorts(before)[usageCohortKey{TaskID: "task", Date: dateOnly(now)}].Unattributed != 1 {
		t.Fatal("native-only work unexpectedly attributed")
	}
	proxyAt := now.Add(-time.Second)
	recordFoldProxy(t, ledger, "late-proxy", "task", proxyAt, 10, 2)
	after := refreshTestFold(t, ledger, now)
	if after == before {
		t.Fatal("late proxy did not refold native-only work")
	}
	task := testFoldTasks(t, ledger, after)["task"]
	if task.Tokens != 12 || !task.LastCompletedAt.Equal(proxyAt) || len(task.Accounts) != 1 || task.Accounts[0] != "account" || len(after.pending) != 0 {
		t.Fatalf("late proxy result: %+v pending=%d", task, len(after.pending))
	}
}

func TestResetDeletionRefoldsAndRemovesOldUsage(t *testing.T) {
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	observation := sessionUsageObservation{TaskID: "task", ResponseID: "before-reset", At: now.Add(-time.Second), Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 10, OutputTokens: 2}}
	insertFoldNative(t, ledger, observation)
	before := refreshTestFold(t, ledger, now)
	if _, err := ledger.sqlDatabase.Exec(`DELETE FROM native_usage_observations WHERE task_id='task'`); err != nil {
		t.Fatal(err)
	}
	observation.ResponseID = "after-reset"
	observation.Usage = &tokenUsage{InputTokens: 30, OutputTokens: 4}
	insertFoldNative(t, ledger, observation)
	after := refreshTestFold(t, ledger, now)
	if after == before {
		t.Fatal("reset did not refold")
	}
	if task := testFoldTasks(t, ledger, after)["task"]; task.Tokens != 34 {
		t.Fatalf("deleted usage remained counted: %+v", task)
	}
}

func TestUTCDateChangeExpiresOldestDayAndRetainsNewDay(t *testing.T) {
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 23, 59, 59, 0, time.UTC)
	oldest := dateOnly(now).AddDate(0, 0, -29).Add(time.Hour)
	recordFoldProxy(t, ledger, "expired", "claude:expired", oldest, 10, 2)
	recordFoldProxy(t, ledger, "current", "claude:current", now.Add(-time.Second), 20, 3)
	before := refreshTestFold(t, ledger, now)
	if len(foldTestCohorts(before)) != 2 {
		t.Fatal("initial window did not include oldest day")
	}
	after := refreshTestFold(t, ledger, now.Add(2*time.Second))
	if after == before {
		t.Fatal("UTC rollover did not refold")
	}
	if len(foldTestCohorts(after)) != 1 {
		t.Fatalf("expired day retained: %d cohorts", len(foldTestCohorts(after)))
	}
	for key, amount := range foldTestCohorts(after) {
		if key.TaskID != "claude:current" || amount.Tokens != 23 {
			t.Fatalf("wrong retained usage: %+v %+v", key, amount)
		}
	}
}

func TestLateParentAndTurnOwnerRollUpExistingWorkWithoutRefold(t *testing.T) {
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	insertFoldNative(t, ledger, sessionUsageObservation{TaskID: "child", ResponseID: "child-work", RootTurnID: "root-turn", At: now.Add(-3 * time.Second), Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 10, OutputTokens: 2}})
	before := refreshTestFold(t, ledger, now)
	if task := testFoldTasks(t, ledger, before)["child"]; task.Tokens != 12 {
		t.Fatalf("initial unresolved work: %+v", task)
	}
	insertFoldNative(t, ledger, sessionUsageObservation{TaskID: "parent", ResponseID: "parent-work", TurnID: "root-turn", At: now.Add(-2 * time.Second), Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 20, OutputTokens: 3}})
	afterTurn := refreshTestFold(t, ledger, now)
	if afterTurn != before {
		t.Fatal("late turn owner unnecessarily refolded")
	}
	tasks := testFoldTasks(t, ledger, afterTurn)
	if len(tasks) != 1 || tasks["parent"].Tokens != 35 {
		t.Fatalf("late turn owner failed to move previous work: %+v", tasks)
	}
	insertFoldSource(t, ledger, "origin", now)
	if _, err := ledger.sqlDatabase.Exec(`INSERT INTO native_task_child_records VALUES('origin',1,'parent')`); err != nil {
		t.Fatal(err)
	}
	afterParent := refreshTestFold(t, ledger, now)
	if afterParent != before {
		t.Fatal("late parent unnecessarily refolded")
	}
	tasks = testFoldTasks(t, ledger, afterParent)
	if len(tasks) != 1 || tasks["origin"].Tokens != 35 {
		t.Fatalf("late parent failed to roll up previous work: %+v", tasks)
	}
}

func TestLateAllowanceRefoldsNativeOnlyWorkIntoItsAccount(t *testing.T) {
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	if err := ledger.database.RecordAccountAllowanceWindowIdentity(store.AccountAllowanceWindowIdentity{ProviderAccountID: "provider", DurationSeconds: 5 * 60 * 60, ResetsAt: reset, ObservedAt: now.Add(-time.Minute), UsedPercent: 1}); err != nil {
		t.Fatal(err)
	}
	native := sessionUsageObservation{TaskID: "task", ResponseID: "response", At: now.Add(-2 * time.Second), Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 10, OutputTokens: 2}}
	insertFoldNative(t, ledger, native)
	before := refreshTestFold(t, ledger, now)
	native.SourceOffset = 2
	native.At = now.Add(-time.Second)
	native.Usage = nil
	native.Allowance = &sessionAllowance{LimitID: "codex", PlanType: "pro", Primary: &sessionAllowanceWindow{UsedPercent: 1, WindowMinutes: 300, ResetsAt: reset}}
	insertFoldNative(t, ledger, native)
	after := refreshTestFold(t, ledger, now)
	if after == before {
		t.Fatal("late native-only allowance did not refold")
	}
	task := testFoldTasks(t, ledger, after)["task"]
	if task.Tokens != 12 || len(task.Accounts) != 1 || task.Accounts[0] != "account" {
		t.Fatalf("late allowance attribution: %+v", task)
	}
}

// A new allowance window no counted usage went through changes nothing; a
// second owner for a window that attributed usage takes the attribution away.
func TestOnlyAllowanceWindowsThatAttributedUsageRefold(t *testing.T) {
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	record := func(provider string, resetsAt time.Time) {
		t.Helper()
		if err := ledger.database.RecordAccountAllowanceWindowIdentity(store.AccountAllowanceWindowIdentity{ProviderAccountID: store.ProviderAccountID(provider), DurationSeconds: 5 * 60 * 60, ResetsAt: resetsAt, ObservedAt: now.Add(-time.Minute), UsedPercent: 1}); err != nil {
			t.Fatal(err)
		}
	}
	record("provider", reset)
	insertFoldNative(t, ledger, sessionUsageObservation{TaskID: "task", ResponseID: "response", At: now.Add(-2 * time.Second), Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 10, OutputTokens: 2},
		Allowance: &sessionAllowance{LimitID: "codex", PlanType: "pro", Primary: &sessionAllowanceWindow{UsedPercent: 1, WindowMinutes: 300, ResetsAt: reset}}})
	before := refreshTestFold(t, ledger, now)
	if task := testFoldTasks(t, ledger, before)["task"]; len(task.Accounts) != 1 || task.Accounts[0] != "account" {
		t.Fatalf("attribution through the window: %+v", task)
	}

	record("provider", reset.Add(5*time.Hour))
	if after := refreshTestFold(t, ledger, now); after != before {
		t.Fatal("a window no counted usage went through rebuilt the totals")
	}

	record("another provider", reset)
	after := refreshTestFold(t, ledger, now)
	if after == before {
		t.Fatal("a second owner for a window that attributed usage did not rebuild the totals")
	}
	if task := testFoldTasks(t, ledger, after)["task"]; task.Tokens != 12 || len(task.Accounts) != 0 {
		t.Fatalf("a window with two owners still attributed usage: %+v", task)
	}
}

func TestFutureAllowanceTriggersRefoldWhenItsTimestampArrives(t *testing.T) {
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Second)
	reset := now.Add(time.Hour)
	if err := ledger.database.RecordAccountAllowanceWindowIdentity(store.AccountAllowanceWindowIdentity{ProviderAccountID: "provider", DurationSeconds: 5 * 60 * 60, ResetsAt: reset, ObservedAt: now.Add(-time.Minute), UsedPercent: 1}); err != nil {
		t.Fatal(err)
	}
	native := sessionUsageObservation{TaskID: "task", ResponseID: "response", At: now.Add(-time.Second), Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 10, OutputTokens: 2}}
	insertFoldNative(t, ledger, native)
	native.SourceOffset = 2
	native.At = future
	native.Usage = nil
	native.Allowance = &sessionAllowance{LimitID: "codex", PlanType: "pro", Primary: &sessionAllowanceWindow{UsedPercent: 1, WindowMinutes: 300, ResetsAt: reset}}
	insertFoldNative(t, ledger, native)
	before := refreshTestFold(t, ledger, now)
	if !before.deferredUntil.Equal(future) {
		t.Fatalf("future allowance not scheduled: %s", before.deferredUntil)
	}
	if task := testFoldTasks(t, ledger, before)["task"]; task.Tokens != 12 || len(task.Accounts) != 0 {
		t.Fatalf("future allowance applied early: %+v", task)
	}
	after := refreshTestFold(t, ledger, future.Add(time.Second))
	if after == before {
		t.Fatal("future allowance did not refold when eligible")
	}
	if task := testFoldTasks(t, ledger, after)["task"]; task.Tokens != 12 || len(task.Accounts) != 1 || task.Accounts[0] != "account" {
		t.Fatalf("eligible allowance not applied: %+v", task)
	}
}

func refreshTestFold(t *testing.T, ledger *UsageLedger, now time.Time) *usageFold {
	t.Helper()
	state, err := ledger.refreshUsageFold(context.Background(), now, []accounts.PoolAccount{{Key: "account", AccountID: "provider"}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func testFoldTasks(t *testing.T, ledger *UsageLedger, state *usageFold) map[string]taskTotal {
	t.Helper()
	metadata, err := ledger.foldMetadata(state)
	if err != nil {
		t.Fatal(err)
	}
	totals, err := totalTaskCohorts(state.reportingCohorts(), metadata, state.turnOwnerTask, state.today.AddDate(0, 0, -29), state.today.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]taskTotal{}
	for _, task := range totals {
		result[task.TaskID] = task
	}
	return result
}

func insertFoldSource(t *testing.T, ledger *UsageLedger, task string, at time.Time) {
	t.Helper()
	var exists bool
	if err := ledger.sqlDatabase.QueryRow(`SELECT EXISTS(SELECT 1 FROM native_usage_sources WHERE task_id=?)`, task).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		return
	}
	source := sessionFile{TaskID: task, Path: filepath.Join(t.TempDir(), task+".jsonl"), Identity: "test", Size: 1, ModifiedAt: at}
	if err := ledger.saveSessionFile(context.Background(), source, sessionReadResult{Metadata: taskMetadata{TaskID: task, CreatedAt: at}}, true, nil); err != nil {
		t.Fatal(err)
	}
}

func insertFoldNative(t *testing.T, ledger *UsageLedger, observation sessionUsageObservation) {
	t.Helper()
	insertFoldSource(t, ledger, observation.TaskID, observation.At)
	arguments := []any{observation.TaskID, observation.SourceOffset, observation.ResponseID, observation.RootTurnID, observation.SessionID, observation.TurnID, observation.At.UnixNano(), observation.Model, observation.ServiceTier}
	arguments = append(arguments, tokenArguments(observation.Usage)...)
	if allowance := observation.Allowance; allowance != nil {
		arguments = append(arguments, allowance.LimitID, allowance.PlanType)
		arguments = append(arguments, windowArguments(allowance.Primary)...)
		arguments = append(arguments, windowArguments(allowance.Secondary)...)
	} else {
		arguments = append(arguments, nil, nil, nil, nil, nil, nil, nil, nil)
	}
	if _, err := ledger.sqlDatabase.Exec(`INSERT INTO native_usage_observations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, arguments...); err != nil {
		t.Fatal(err)
	}
}

func recordFoldProxy(t *testing.T, ledger *UsageLedger, responseID, task string, at time.Time, input, output int64) {
	t.Helper()
	if err := ledger.database.RecordCompletedResponse(store.CompletedResponseRecord{CompletedResponseID: store.CompletedResponseID(responseID), TaskID: store.TaskID(task), AccountKey: "account", ModelSlug: "gpt-5.6-luna", HarnessMode: "standard", InputTokens: input, OutputTokens: output, CompletedAt: at}); err != nil {
		t.Fatal(err)
	}
}

func foldTestCohorts(state *usageFold) map[usageCohortKey]usageCohortAmount {
	cohorts := make(map[usageCohortKey]usageCohortAmount)
	for key, amount := range state.reportingCohorts() {
		addCohort(cohorts, key, amount)
	}
	return cohorts
}

func TestChildRecordsNeedNativeSourceBeforeRollingUpProxyTask(t *testing.T) {
	ledger := openTestUsageLedger(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	insertFoldSource(t, ledger, "origin", now)
	recordFoldProxy(t, ledger, "unknown-response", "unknown", now.Add(-time.Second), 10, 2)
	recordFoldProxy(t, ledger, "claude-response", "claude:code:session", now.Add(-time.Second), 20, 3)
	if _, err := ledger.sqlDatabase.Exec(`INSERT INTO native_task_child_records VALUES('origin',1,'unknown'),('origin',2,'claude:code:session')`); err != nil {
		t.Fatal(err)
	}
	before := refreshTestFold(t, ledger, now)
	tasks := testFoldTasks(t, ledger, before)
	if len(tasks) != 2 || tasks["unknown"].Tokens != 12 || tasks["claude:code:session"].Tokens != 23 {
		t.Fatalf("child record applied without native source: %+v", tasks)
	}
	insertFoldSource(t, ledger, "unknown", now)
	after := refreshTestFold(t, ledger, now)
	if after != before {
		t.Fatal("late native metadata unnecessarily refolded")
	}
	tasks = testFoldTasks(t, ledger, after)
	if len(tasks) != 2 || tasks["origin"].Tokens != 12 || tasks["claude:code:session"].Tokens != 23 {
		t.Fatalf("late native source did not apply the child record: %+v", tasks)
	}
}

func TestLocalMidnightResetsProxyActivityWithoutMovingUTCTokens(t *testing.T) {
	ledger := openTestUsageLedger(t)
	// A fixed offset makes local midnight differ from the UTC date boundary.
	previousLocal := time.Local
	time.Local = time.FixedZone("test local", 3*60*60)
	t.Cleanup(func() { time.Local = previousLocal })
	now := time.Date(2026, 10, 22, 20, 59, 59, 0, time.UTC)
	recordFoldProxy(t, ledger, "previous-local-day", "claude:previous", now.Add(-time.Second), 10, 2)
	before := refreshTestFold(t, ledger, now)
	if len(before.proxyActivity) != 1 {
		t.Fatal("today's proxy activity missing")
	}
	after := refreshTestFold(t, ledger, now.Add(2*time.Second))
	if after == before || len(after.proxyActivity) != 0 {
		t.Fatal("local midnight did not reset today's proxy activity")
	}
	if task := testFoldTasks(t, ledger, after)["claude:previous"]; task.Tokens != 12 {
		t.Fatalf("local midnight changed UTC reporting: %+v", task)
	}
}

func TestLateProxyRetainsLegacyNativeOverlapError(t *testing.T) {
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	insertFoldNative(t, ledger, sessionUsageObservation{TaskID: "legacy", At: now.Add(-time.Second), Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 10, OutputTokens: 2}})
	refreshTestFold(t, ledger, now)
	recordFoldProxy(t, ledger, "overlapping", "legacy", now.Add(-2*time.Second), 20, 3)
	if _, err := ledger.refreshUsageFold(context.Background(), now, []accounts.PoolAccount{{Key: "account", AccountID: "provider"}}, time.Time{}); err == nil {
		t.Fatal("late proxy silently counted overlapping legacy native usage")
	}
}
