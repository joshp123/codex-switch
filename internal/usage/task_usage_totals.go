package usage

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/codexstate"
	"github.com/joshp123/codex-switch/internal/menupb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ReadUsage imports changed session bytes once and reads all usage figures
// from one incremental fold. Returned reports belong to the caller.
func (ledger *UsageLedger) ReadUsage(ctx context.Context, accountKeys []string) ([]*menupb.ReportingPeriod, []*menupb.AccountDay, *menupb.ProxyActivity, error) {
	ledger.usageMutex.Lock()
	defer ledger.usageMutex.Unlock()
	now := time.Now().UTC()
	start := dateOnly(now).AddDate(0, 0, 1-reportingPeriodDays)
	reader, err := codexstate.DefaultReader()
	if err != nil {
		return nil, nil, nil, err
	}
	piSessions, err := piSessionDirectory()
	if err != nil {
		return nil, nil, nil, err
	}
	if err = ledger.importNativeSessions(ctx, reader.CodexHome, piSessions, start); err != nil {
		return nil, nil, nil, err
	}
	return ledger.readUsageReports(ctx, accountKeys, now)
}

func reportUsagePeriods(today time.Time, pool []accounts.PoolAccount, cohorts iter.Seq2[usageCohortKey, usageCohortAmount], metadata taskMetadataLookup, turnOwners func(string) string, dailyUsage map[accountDay]usageAmount) ([]*menupb.ReportingPeriod, []*menupb.AccountDay, error) {
	var days []*menupb.AccountDay
	for _, account := range pool {
		for _, date := range periodDates(today, reportingPeriodDays) {
			startedAt, err := time.Parse(time.DateOnly, date)
			if err != nil {
				return nil, nil, err
			}
			amount := dailyUsage[accountDay{string(account.Key), date}]
			cost := int64(amount.cost)
			days = append(days, &menupb.AccountDay{AccountKey: string(account.Key), StartedAt: timestamppb.New(startedAt), Tokens: amount.tokens, CostNanoUsd: &cost})
		}
	}
	definitions := []struct {
		kind menupb.ReportingPeriodKind
		days int
	}{
		{menupb.ReportingPeriodKind_REPORTING_PERIOD_TODAY, 1},
		{menupb.ReportingPeriodKind_REPORTING_PERIOD_SEVEN_DAYS, 7},
		{menupb.ReportingPeriodKind_REPORTING_PERIOD_THIRTY_DAYS, reportingPeriodDays},
	}
	starts := make([]time.Time, len(definitions))
	for index, definition := range definitions {
		starts[index] = today.AddDate(0, 0, 1-definition.days)
	}
	end := today.AddDate(0, 0, 1)
	taskPeriods, err := totalTaskCohortPeriods(cohorts, metadata, turnOwners, starts, end)
	if err != nil {
		return nil, nil, err
	}
	periods := make([]*menupb.ReportingPeriod, 0, len(definitions))
	for index, definition := range definitions {
		begin := starts[index]
		period := &menupb.ReportingPeriod{Kind: definition.kind, StartedAt: timestamppb.New(begin), EndedAt: timestamppb.New(end), TaskUsage: &menupb.TaskUsageReport{}}
		for _, account := range pool {
			var total usageAmount
			for _, date := range periodDates(today, definition.days) {
				amount := dailyUsage[accountDay{string(account.Key), date}]
				total.tokens += amount.tokens
				total.cost += amount.cost
			}
			cost := int64(total.cost)
			period.Accounts = append(period.Accounts, &menupb.AccountTotal{AccountKey: string(account.Key), Tokens: total.tokens, CostNanoUsd: &cost})
		}
		for _, total := range taskPeriods[index] {
			period.TaskUsage.Tasks = append(period.TaskUsage.Tasks, completedTask(total))
		}
		periods = append(periods, period)
	}
	return periods, days, nil
}

// Account days merge the provider report with attributed local work. Standard
// proxy prices are folded independently because valuation excludes Fast work.
func (ledger *UsageLedger) accountDailyUsageFromHistories(pool []accounts.PoolAccount, cohorts iter.Seq2[usageCohortKey, usageCohortAmount], credits map[string][]accounts.OpenAIModelDayCredits, localStandardCost map[string]map[modelDay]nanoUSD, histories map[string]accounts.OpenAIDailyTokenHistory) map[accountDay]usageAmount {
	local := make(map[string]map[string]usageAmount)
	for key, contribution := range cohorts {
		account := string(key.Account)
		if account == "" {
			continue
		}
		if local[account] == nil {
			local[account] = make(map[string]usageAmount)
		}
		date := key.Date.Format(time.DateOnly)
		amount := local[account][date]
		amount.tokens += contribution.Tokens
		amount.cost += contribution.Cost
		local[account][date] = amount
	}
	daily := make(map[accountDay]usageAmount)
	for _, account := range pool {
		if account.Provider == accounts.ProviderClaude {
			for date, amount := range local[string(account.Key)] {
				daily[accountDay{string(account.Key), date}] = amount
			}
			continue
		}
		openAITokens := make(map[string]int64)
		for _, bucket := range histories[account.AccountID].Buckets {
			openAITokens[bucket.StartDate] = bucket.Tokens
		}
		accountCredits := credits[account.AccountID]
		value, agreeing := dollarsPerPercent(accountCredits, localStandardCost[string(account.Key)])
		if value > 0 {
			ledger.logOnChange("value/"+string(account.Key), fmt.Sprintf("%s: OpenAI's busiest day in the last 30 days is worth $%.2f at API prices, agreed by %d model-days served only through this Mac", account.DisplayName, value/1e7, agreeing))
		} else if len(accountCredits) > 0 {
			ledger.logOnChange("value/"+string(account.Key), fmt.Sprintf("%s: cannot price the use OpenAI reports from elsewhere: fewer than two model-days in the last 30 days were served only through this Mac", account.DisplayName))
		}
		for date, amount := range accountDailyUsage(openAITokens, openAIDailyCost(accountCredits, value), local[string(account.Key)]) {
			daily[accountDay{string(account.Key), date}] = amount
		}
	}
	return daily
}

func piSessionDirectory() (string, error) {
	if directory := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); directory != "" {
		return directory, nil
	}
	piDirectory := os.Getenv("PI_CODING_AGENT_DIR")
	if piDirectory == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		piDirectory = filepath.Join(home, ".pi", "agent")
	}
	return filepath.Join(piDirectory, "sessions"), nil
}

func completedTask(total taskTotal) *menupb.CompletedTask {
	tokens := total.Tokens
	task := &menupb.CompletedTask{TaskId: total.TaskID, Tokens: &tokens, LastCompletedAt: timestamppb.New(total.LastCompletedAt)}
	if total.Title != "" {
		title := total.Title
		task.Title = &title
	}
	if total.Cost != nil {
		cost := int64(*total.Cost)
		task.CostNanoUsd = &cost
	}
	for _, account := range total.Accounts {
		task.AccountKeys = append(task.AccountKeys, string(account))
	}
	return task
}
