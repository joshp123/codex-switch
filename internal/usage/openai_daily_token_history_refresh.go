package usage

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
)

// refreshOpenAIDailyTokenHistories reads every pool account's daily token
// history and daily usage by model from OpenAI, independently of routing and
// credential refresh.
func (ledger *UsageLedger) refreshOpenAIDailyTokenHistories(ctx context.Context) {
	pool, err := accounts.ReadPool(ledger.dataDirectory)
	if err != nil {
		ledger.logOnChange("", fmt.Sprintf("could not read the account pool: %v", err))
		return
	}
	ledger.logOnChange("", "")
	var workers sync.WaitGroup
	for _, account := range pool {
		if account.Provider == accounts.ProviderClaude {
			continue
		}
		workers.Add(1)
		go func(account accounts.PoolAccount) {
			defer workers.Done()
			requestContext, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			profileSource := accounts.NewOpenAIDailyTokenSource(filepath.Join(ledger.dataDirectory, account.Name), ledger.upstreamOrigin, ledger.httpClient)
			history, err := profileSource.Read(requestContext)
			if err != nil {
				if ctx.Err() == nil {
					ledger.logOnChange(account.Name, fmt.Sprintf("%s: could not read OpenAI's daily token history: %v", account.DisplayName, err))
				}
				return
			}
			if history.AccountID != account.AccountID {
				ledger.logOnChange(account.Name, fmt.Sprintf("%s: OpenAI's daily token history belongs to a different account than its saved login", account.DisplayName))
				return
			}
			if _, err = ledger.saveOpenAIDailyTokenHistory(history); err != nil {
				ledger.logOnChange(account.Name, fmt.Sprintf("%s: could not save OpenAI's daily token history: %v", account.DisplayName, err))
				return
			}
			lastDate := time.Now().UTC()
			firstDate := lastDate.AddDate(0, 0, 1-reportingPeriodDays)
			creditsAccountID, credits, err := profileSource.ReadDailyModelCredits(requestContext, firstDate, lastDate)
			if err != nil {
				if ctx.Err() == nil {
					ledger.logOnChange(account.Name, fmt.Sprintf("%s: could not read OpenAI's daily usage by model: %v", account.DisplayName, err))
				}
				return
			}
			if creditsAccountID != account.AccountID {
				ledger.logOnChange(account.Name, fmt.Sprintf("%s: OpenAI's daily usage by model belongs to a different account than its saved login", account.DisplayName))
				return
			}
			if err = ledger.saveOpenAIModelDayCredits(account.AccountID, credits); err != nil {
				ledger.logOnChange(account.Name, fmt.Sprintf("%s: could not save OpenAI's daily usage by model: %v", account.DisplayName, err))
				return
			}
			ledger.logOnChange(account.Name, "")
		}(account)
	}
	workers.Wait()
}

// logOnChange logs line when it differs from the last line logged under key,
// so a lasting condition is one line, not one a minute. An empty line records
// recovery silently.
func (ledger *UsageLedger) logOnChange(key, line string) {
	ledger.logMutex.Lock()
	defer ledger.logMutex.Unlock()
	if ledger.lastLoggedFor[key] == line {
		return
	}
	ledger.lastLoggedFor[key] = line
	if line != "" {
		log.Print(line)
	}
}
