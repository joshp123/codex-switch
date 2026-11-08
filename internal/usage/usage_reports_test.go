package usage

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/menupb"
	"github.com/joshp123/codex-switch/internal/store"
)

func TestUsageReportsUseEndOfTodayAndKeepEarlierResultsIndependent(t *testing.T) {
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	insertFoldNative(t, ledger, sessionUsageObservation{TaskID: "task", ResponseID: "first", At: now.Add(-time.Second), Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 10, OutputTokens: 2}})
	first, _, activity, err := ledger.readUsageReports(context.Background(), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Second)
	second, _, nextActivity, err := ledger.readUsageReports(context.Background(), nil, later)
	if err != nil {
		t.Fatal(err)
	}
	endOfToday := dateOnly(now).AddDate(0, 0, 1)
	if !second[0].EndedAt.AsTime().Equal(endOfToday) || !first[0].EndedAt.AsTime().Equal(endOfToday) || !nextActivity.EndedAt.AsTime().Equal(later) || !activity.EndedAt.AsTime().Equal(now) {
		t.Fatal("a period must end at the end of today, and the activity's fresh end time must not change an earlier snapshot")
	}
	insertFoldNative(t, ledger, sessionUsageObservation{TaskID: "task", ResponseID: "second", SourceOffset: 1, At: now, Model: "gpt-5.6-luna", Usage: &tokenUsage{InputTokens: 20, OutputTokens: 3}})
	third, _, _, err := ledger.readUsageReports(context.Background(), nil, later)
	if err != nil {
		t.Fatal(err)
	}
	if third[0].TaskUsage.Tasks[0].GetTokens() != 35 || first[0].TaskUsage.Tasks[0].GetTokens() != 12 {
		t.Fatal("new work was omitted or changed the earlier snapshot")
	}
}

// The proxy reads the Mac's time zone once, when it starts. Activity through
// the proxy must cover the same UTC day as Today, whatever that zone was and
// after the Mac moves to another.
func TestProxyActivityCoversTheUTCDayWhateverTheMacsZone(t *testing.T) {
	previousLocal := time.Local
	t.Cleanup(func() { time.Local = previousLocal })
	time.Local = time.FixedZone("Athens summer", 3*60*60)
	ledger := openTestUsageLedger(t)
	now := time.Date(2026, 10, 22, 21, 30, 0, 0, time.UTC) // 00:30 on 23 October in Athens
	recordFoldProxy(t, ledger, "athens-evening", "claude:evening", time.Date(2026, 10, 22, 20, 30, 0, 0, time.UTC), 10, 2)
	recordFoldProxy(t, ledger, "athens-after-midnight", "claude:night", time.Date(2026, 10, 22, 21, 15, 0, 0, time.UTC), 20, 3)
	recordFoldProxy(t, ledger, "previous-utc-day", "claude:yesterday", time.Date(2026, 10, 21, 23, 0, 0, 0, time.UTC), 40, 4)
	read := func(at time.Time) *menupb.ProxyActivity {
		t.Helper()
		periods, _, activity, err := ledger.readUsageReports(context.Background(), nil, at)
		if err != nil {
			t.Fatal(err)
		}
		var today int64
		for _, task := range periods[0].TaskUsage.Tasks {
			today += task.GetTokens()
		}
		var tokens int64
		for _, account := range activity.Accounts {
			tokens += account.Tokens
		}
		if !activity.StartedAt.AsTime().Equal(dateOnly(at)) || !activity.StartedAt.AsTime().Equal(periods[0].StartedAt.AsTime()) || tokens != today {
			t.Fatalf("at %s with the Mac on %s, activity from %s has %d tokens; Today from %s has %d",
				at, time.Local, activity.StartedAt.AsTime(), tokens, periods[0].StartedAt.AsTime(), today)
		}
		return activity
	}
	if tokens := read(now).Accounts[0].Tokens; tokens != 35 {
		t.Fatalf("activity on 22 October UTC has %d tokens, want 35", tokens)
	}
	time.Local = time.FixedZone("California winter", -8*60*60) // the Mac moves while the proxy runs
	if tokens := read(now.Add(time.Minute)).Accounts[0].Tokens; tokens != 35 {
		t.Fatalf("after the Mac changed zone, activity has %d tokens, want 35", tokens)
	}
	if accounts := read(time.Date(2026, 10, 23, 0, 0, 1, 0, time.UTC)).Accounts; len(accounts) != 0 {
		t.Fatalf("activity did not start again at midnight UTC: %v", accounts)
	}
}

func TestUsageReportsRefreshTitleAndRequestCountsWithoutNewUsage(t *testing.T) {
	ledger := openTestUsageLedger(t)
	directory := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", directory)
	registry := filepath.Join(directory, "sessions", "123.json")
	writeClaudeRegistryFixture(t, registry, "session", "", 1)
	project := filepath.Join(directory, "projects", "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "session.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"custom-title\",\"customTitle\":\"First\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	recordFoldProxy(t, ledger, "response", "claude:code:session", now.Add(-time.Second), 10, 2)
	first, _, _, err := ledger.readUsageReports(context.Background(), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	state, marks := ledger.fold, ledger.fold.marks
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("{\"type\":\"custom-title\",\"customTitle\":\"Second title\"}\n")
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("append title: %v %v", err, closeErr)
	}
	writeClaudeRegistryFixture(t, registry, "session", "", 2)
	second, _, _, err := ledger.readUsageReports(context.Background(), nil, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if ledger.fold != state || ledger.fold.marks != marks || second[0].TaskUsage.Tasks[0].GetTitle() != "Second title" || first[0].TaskUsage.Tasks[0].GetTitle() != "First" {
		t.Fatal("a title-only edit did not refresh reports independently of usage rows")
	}
	if err := ledger.database.StartModelRequestObservation(now); err != nil {
		t.Fatal(err)
	}
	if err := ledger.database.RecordModelRequestOutcome("account", now, store.ModelRequestStarted); err != nil {
		t.Fatal(err)
	}
	_, _, activity, err := ledger.readUsageReports(context.Background(), []string{"account", "idle"}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Accounts) != 2 || activity.Accounts[0].RequestCounts.Requests != 1 || activity.Accounts[1].AccountKey != "idle" || !activity.RequestsStartedAt.AsTime().Equal(now) || ledger.fold != state || ledger.fold.marks != marks {
		t.Fatal("an admitted request or a new empty account was omitted without a completion")
	}
}

func TestUsageReportsRefreshProviderTokensAndValuationWithoutNewUsage(t *testing.T) {
	ledger := openTestUsageLedger(t)
	accountDirectory := filepath.Join(ledger.dataDirectory, "account-1")
	if err := os.Mkdir(accountDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"usage-test@example.invalid"}`))
	if err := os.WriteFile(filepath.Join(accountDirectory, "auth.json"), []byte(`{"tokens":{"account_id":"provider","id_token":"test.`+claims+`.test"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.RecordAccountKeys(ledger.dataDirectory, ledger.database); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, days := range []int{-2, -1} {
		at := now.AddDate(0, 0, days)
		if err := ledger.database.RecordCompletedResponse(store.CompletedResponseRecord{CompletedResponseID: store.CompletedResponseID(at.Format(time.DateOnly)), TaskID: "task", AccountKey: "chatgpt:usage-test@example.invalid", ModelSlug: "gpt-5.6-luna", HarnessMode: "standard", InputTokens: 1000, OutputTokens: 100, CompletedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	credits := []accounts.OpenAIModelDayCredits{}
	for _, days := range []int{-2, -1, 0} {
		credits = append(credits, accounts.OpenAIModelDayCredits{Date: now.AddDate(0, 0, days).Format(time.DateOnly), Model: "gpt-5.6-luna", PercentOfBusiestDay: 1})
	}
	if err := ledger.saveOpenAIModelDayCredits("provider", credits); err != nil {
		t.Fatal(err)
	}
	history := accounts.OpenAIDailyTokenHistory{AccountID: "provider", ReadStartedAt: now, GeneratedAt: now, ReportingDate: dateOnly(now), Buckets: []accounts.OpenAIDailyTokenBucket{{StartDate: now.Format(time.DateOnly), Tokens: 10}}}
	if _, err := ledger.saveOpenAIDailyTokenHistory(history); err != nil {
		t.Fatal(err)
	}
	read := func() *menupb.AccountTotal {
		periods, _, _, err := ledger.readUsageReports(context.Background(), nil, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return periods[0].Accounts[0]
	}
	before := read()
	state, marks := ledger.fold, ledger.fold.marks
	history.ReadStartedAt = now.Add(time.Second)
	history.Buckets[0].Tokens = 20
	if _, err := ledger.saveOpenAIDailyTokenHistory(history); err != nil {
		t.Fatal(err)
	}
	credits[2].PercentOfBusiestDay = 2
	if err := ledger.saveOpenAIModelDayCredits("provider", credits); err != nil {
		t.Fatal(err)
	}
	after := read()
	if before.Tokens != 10 || after.Tokens != 20 || before.GetCostNanoUsd() <= 0 || after.GetCostNanoUsd() != 2*before.GetCostNanoUsd() || ledger.fold != state || ledger.fold.marks != marks {
		t.Fatalf("provider-only update: before=%v after=%v", before, after)
	}
}
