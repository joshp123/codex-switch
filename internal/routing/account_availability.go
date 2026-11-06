package routing

import (
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

// accountUnavailableReason is the one rule for whether an account may take new
// model work. It returns "" when the account can serve modelSlug, otherwise the
// reason in the words the status command and the proxy log show.
func (router *TaskRouter) accountUnavailableReason(fact accounts.AccountRoutingFact, modelSlug string) (string, error) {
	if problem := accountAccessProblem(fact); problem != "" {
		return problem, nil
	}
	now := router.currentTime().UTC()
	if reason := accounts.ReserveUnavailableReason(fact, now); reason != "" {
		return reason, nil
	}
	if fact.Provider == accounts.ProviderClaude {
		reason, resetsAt := accounts.ClaudeUnavailableReason(fact, modelSlug, now)
		if reason != "" {
			reason += resetSuffix(resetsAt)
		}
		return reason, nil
	}
	for _, kind := range []accounts.AllowanceWindowKind{accounts.FiveHours, accounts.Weekly} {
		window, known := accounts.AnalyseAllowance(fact.AllowanceSample, fact.AccountState, now, kind)
		if known && window.FreePercent <= 0 {
			return fmt.Sprintf("%s allowance used up%s", kind, resetSuffix(window.ResetsAt)), nil
		}
	}
	if fact.AccountState == accounts.AccountReady && fact.AllowanceSample.Allowed && !fact.AllowanceSample.LimitReached {
		// A provider read begun before exhaustion cannot supersede that exhaustion.
		cleared, err := router.storeDatabase.ClearAccountExhaustionBefore(fact.AccountName, fact.RefreshedAt)
		if err != nil {
			return "", err
		}
		if cleared {
			log.Printf("%s can take new work again: OpenAI's allowance reading at %s shows it is no longer limited",
				fact.DisplayName, fact.AllowanceSample.ObservedAt.Local().Format("15:04:05"))
		}
	}
	resetsAt, exhausted, err := router.storeDatabase.ReadAccountReportedReset(fact.AccountName)
	if err != nil {
		return "", err
	}
	if exhausted {
		return "hit its usage limit" + resetSuffix(resetsAt), nil
	}
	if modelSlug != "" && fact.SupportedModelSlugs == nil {
		return "its model list has not been read yet", nil
	}
	if !accountSupportsModel(fact, modelSlug) {
		return "does not offer " + modelSlug, nil
	}
	return "", nil
}

// accountAccessProblem covers what no allowance reset fixes, and the OpenAI
// limit state. It returns "" when only allowance and reserve remain to check.
func accountAccessProblem(fact accounts.AccountRoutingFact) string {
	switch {
	case fact.AccountState == accounts.AccountSignInRequired:
		command := "codex-switch login "
		if fact.Provider == accounts.ProviderClaude {
			command += "--claude "
		}
		return "signed out; run: " + command + fmt.Sprintf("%q", fact.DisplayName)
	case !accountCredentialsUsable(fact.Credentials) || fact.AccountState == accounts.AccountCredentialsUnavailable:
		return withCause("cannot read its saved login", fact.AccountStateCause)
	case fact.Provider != accounts.ProviderClaude && fact.AccountState == accounts.AccountLimited:
		return "OpenAI reports its usage limit reached" + resetSuffix(latestReset(fact.AllowanceSample))
	case !fact.AccountState.Eligible() && !(fact.Provider == accounts.ProviderClaude && fact.AccountState == accounts.AccountLimited):
		return "account state " + string(fact.AccountState)
	}
	return ""
}

// claudePoolAvailableAgain is the earliest time a Claude account that can be
// used at all is freed by an allowance reset, with the limit that ends then.
func (router *TaskRouter) claudePoolAvailableAgain(facts []accounts.AccountRoutingFact, modelSlug string) (time.Time, string) {
	var earliest time.Time
	earliestClaim := ""
	for _, fact := range facts {
		if accountAccessProblem(fact) != "" {
			continue
		}
		availableAt, claim, found := accounts.ClaudeAvailableAgain(fact, modelSlug, router.currentTime().UTC())
		if found && (earliest.IsZero() || availableAt.Before(earliest)) {
			earliest, earliestClaim = availableAt, claim
		}
	}
	return earliest, earliestClaim
}

// weeklyAllowance is the weekly window that ranks accounts for new work. An
// account that reports no weekly window ranks last.
func (router *TaskRouter) weeklyAllowance(fact accounts.AccountRoutingFact, modelSlug string) (accounts.AllowanceWindow, bool) {
	if fact.Provider == accounts.ProviderClaude {
		return accounts.ClaudeWeeklyAllowance(fact, modelSlug, router.currentTime().UTC())
	}
	return accounts.AnalyseAllowance(fact.AllowanceSample, fact.AccountState, router.currentTime().UTC(), accounts.Weekly)
}

func accountCredentialsUsable(credentials accounts.Credentials) bool {
	return strings.TrimSpace(credentials.AccessToken) != "" && strings.TrimSpace(credentials.AccountID) != ""
}

func accountSupportsModel(fact accounts.AccountRoutingFact, modelSlug string) bool {
	return modelSlug == "" || slices.Contains(fact.SupportedModelSlugs, modelSlug)
}

func factForAccount(facts []accounts.AccountRoutingFact, accountName store.AccountName) (accounts.AccountRoutingFact, bool) {
	for _, fact := range facts {
		if fact.AccountName == accountName {
			return fact, true
		}
	}
	return accounts.AccountRoutingFact{}, false
}

func latestReset(sample accounts.AllowanceSample) time.Time {
	var latest time.Time
	for _, window := range sample.Windows {
		if window.UsedPercent >= 100 && window.ResetsAt.After(latest) {
			latest = window.ResetsAt
		}
	}
	return latest
}

func resetSuffix(resetsAt time.Time) string {
	if resetsAt.IsZero() {
		return ""
	}
	return " until " + resetsAt.Local().Format("Mon 2 Jan 15:04")
}

func withCause(summary string, cause error) string {
	if cause == nil {
		return summary
	}
	return summary + ": " + cause.Error()
}
