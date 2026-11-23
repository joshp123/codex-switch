package usage

import (
	"fmt"
	"math"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
)

// reportingPeriodDays is the longest reporting period, 30 Days.
const reportingPeriodDays = 30

// Included Fast usage draws 2.5 times the Standard allowance
// (https://learn.chatgpt.com/docs/pricing); its API-equivalent cost is twice
// the Standard cost.
const fastAllowanceMultiplier = 2.5

// usageAmount is tokens (input plus output) and their API-equivalent cost.
type usageAmount struct {
	tokens int64
	cost   nanoUSD
}

// accountDay is one account's usage on one UTC date.
type accountDay struct {
	account string
	date    string
}

// modelDay is one model at one speed on one UTC date.
type modelDay struct {
	date  string
	model string
	fast  bool
}

// dollarsPerPercent is what one percent of the busiest day in an account's
// latest OpenAI read is worth at API-equivalent prices, in nano-US dollars.
// OpenAI reports each model's daily credits only relative to that day. On a
// day when all of one model's Standard use on the account went through this
// Mac, this Mac's exact cost divided by OpenAI's percentage is the value, and
// the same value repeats on every such day; days with use elsewhere give less.
// The value is the one most model-days agree on, to seven significant figures,
// and at least two must agree. It depends only on OpenAI's published credit
// rates, never on the size of the plan's allowance.
func dollarsPerPercent(credits []accounts.OpenAIModelDayCredits, localStandardCost map[modelDay]nanoUSD) (float64, int) {
	agreeing := make(map[string][]float64)
	for _, day := range credits {
		cost := localStandardCost[modelDay{day.Date, day.Model, false}]
		if day.Fast || cost <= 0 || day.PercentOfBusiestDay <= 0 {
			continue
		}
		value := float64(cost) / day.PercentOfBusiestDay
		key := fmt.Sprintf("%.6e", value)
		agreeing[key] = append(agreeing[key], value)
	}
	var best []float64
	for _, values := range agreeing {
		if len(values) > len(best) || (len(values) == len(best) && len(best) > 0 && values[0] > best[0]) {
			best = values
		}
	}
	if len(best) < 2 {
		return 0, len(best)
	}
	return best[0], len(best)
}

// openAIDailyCost values each UTC date's model credits at valuePerPercent.
func openAIDailyCost(credits []accounts.OpenAIModelDayCredits, valuePerPercent float64) map[string]nanoUSD {
	costs := make(map[string]nanoUSD)
	for _, day := range credits {
		value := day.PercentOfBusiestDay * valuePerPercent
		if day.Fast {
			value = value / fastAllowanceMultiplier * fastCostMultiplier
		}
		costs[day.Date] += nanoUSD(math.Round(value))
	}
	return costs
}

// accountDailyUsage merges OpenAI's report with this Mac's record for each
// UTC date. Each counts only part of a day that is still in progress: OpenAI
// counts every machine but reports tokens about 12 hours late and model
// shares one to two days late, and this Mac counts only its own work. Neither
// can exceed the account's real use, so each date takes the larger, which is
// OpenAI's once it has caught up.
func accountDailyUsage(openAITokens map[string]int64, openAICost map[string]nanoUSD, local map[string]usageAmount) map[string]usageAmount {
	merged := make(map[string]usageAmount)
	for date, tokens := range openAITokens {
		merged[date] = usageAmount{tokens: tokens}
	}
	for date, cost := range openAICost {
		amount := merged[date]
		amount.cost = cost
		merged[date] = amount
	}
	for date, amount := range local {
		merged[date] = usageAmount{tokens: max(merged[date].tokens, amount.tokens), cost: max(merged[date].cost, amount.cost)}
	}
	return merged
}

// periodDates lists the UTC dates of a period of the given length ending today.
func periodDates(today time.Time, days int) []string {
	dates := make([]string, 0, days)
	for offset := days - 1; offset >= 0; offset-- {
		dates = append(dates, today.AddDate(0, 0, -offset).Format(time.DateOnly))
	}
	return dates
}

func dateOnly(at time.Time) time.Time {
	at = at.UTC()
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
}
