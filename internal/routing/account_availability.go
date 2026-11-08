package routing

import (
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/logtime"
	"github.com/joshp123/codex-switch/internal/store"
)

// AccountUnavailability is why an account cannot take new model work, and when
// that ends if a reset is known. Reason holds no time, so each reader writes
// Until in its own time zone: the proxy log in UTC, status and the menu in the
// Mac's current zone. An empty Reason means the account can take the work.
type AccountUnavailability struct {
	Reason string
	Until  time.Time
}

// LogText is the reason as the proxy log writes it.
func (unavailability AccountUnavailability) LogText() string {
	return unavailability.Reason + resetSuffix(unavailability.Until)
}

// accountUnavailability is the one rule for whether an account may take new
// model work for modelSlug.
func (router *TaskRouter) accountUnavailability(fact accounts.AccountRoutingFact, modelSlug string) (AccountUnavailability, error) {
	if problem := accountAccessProblem(fact); problem.Reason != "" {
		return problem, nil
	}
	now := router.currentTime().UTC()
	if reason := accounts.ReserveUnavailableReason(fact, now); reason != "" {
		return AccountUnavailability{Reason: reason}, nil
	}
	if fact.Provider == accounts.ProviderClaude {
		reason, resetsAt := accounts.ClaudeUnavailableReason(fact, modelSlug, now)
		return AccountUnavailability{Reason: reason, Until: resetsAt}, nil
	}
	for _, kind := range []accounts.AllowanceWindowKind{accounts.FiveHours, accounts.Weekly} {
		window, known := accounts.AnalyseAllowance(fact.AllowanceSample, fact.AccountState, now, kind)
		if known && window.FreePercent <= 0 {
			return AccountUnavailability{Reason: fmt.Sprintf("%s allowance used up", kind), Until: window.ResetsAt}, nil
		}
	}
	if fact.AccountState == accounts.AccountReady && fact.AllowanceSample.Allowed && !fact.AllowanceSample.LimitReached {
		// A provider read begun before exhaustion cannot supersede that exhaustion.
		cleared, err := router.storeDatabase.ClearAccountExhaustionBefore(fact.AccountKey, fact.RefreshedAt)
		if err != nil {
			return AccountUnavailability{}, err
		}
		if cleared {
			log.Printf("%s can take new work again: OpenAI's allowance reading at %s shows it is no longer limited",
				fact.DisplayName, logtime.Format(fact.AllowanceSample.ObservedAt))
		}
	}
	resetsAt, exhausted, err := router.storeDatabase.ReadAccountReportedReset(fact.AccountKey)
	if err != nil {
		return AccountUnavailability{}, err
	}
	if exhausted {
		return AccountUnavailability{Reason: "hit its usage limit", Until: resetsAt}, nil
	}
	if modelSlug != "" && fact.SupportedModelSlugs == nil {
		return AccountUnavailability{Reason: "its model list has not been read yet"}, nil
	}
	if !accountSupportsModel(fact, modelSlug) {
		return AccountUnavailability{Reason: "does not offer " + modelSlug}, nil
	}
	return AccountUnavailability{}, nil
}

// accountAccessProblem covers what no allowance reset fixes, and the OpenAI
// limit state. Its Reason is empty when only allowance and reserve remain to
// check.
func accountAccessProblem(fact accounts.AccountRoutingFact) AccountUnavailability {
	switch {
	case fact.AccountState == accounts.AccountSignInRequired:
		command := "codex-switch login "
		if fact.Provider == accounts.ProviderClaude {
			command += "--claude "
		}
		return AccountUnavailability{Reason: "signed out; run: " + command + fmt.Sprintf("%q", fact.DisplayName)}
	case !accountCredentialsUsable(fact.Credentials) || fact.AccountState == accounts.AccountCredentialsUnavailable:
		return AccountUnavailability{Reason: withCause("cannot read its saved login", fact.AccountStateCause)}
	case fact.Provider != accounts.ProviderClaude && fact.AccountState == accounts.AccountLimited:
		return AccountUnavailability{Reason: "OpenAI reports its usage limit reached", Until: latestReset(fact.AllowanceSample)}
	case !fact.AccountState.Eligible() && !(fact.Provider == accounts.ProviderClaude && fact.AccountState == accounts.AccountLimited):
		return AccountUnavailability{Reason: "account state " + string(fact.AccountState)}
	}
	return AccountUnavailability{}
}

// claudePoolAvailableAgain is the earliest time a Claude account that can be
// used at all is freed by an allowance reset, with the limit that ends then.
func (router *TaskRouter) claudePoolAvailableAgain(facts []accounts.AccountRoutingFact, modelSlug string) (time.Time, string) {
	var earliest time.Time
	earliestClaim := ""
	for _, fact := range facts {
		if accountAccessProblem(fact).Reason != "" {
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

func factForAccount(facts []accounts.AccountRoutingFact, accountKey store.AccountKey) (accounts.AccountRoutingFact, bool) {
	for _, fact := range facts {
		if fact.AccountKey == accountKey {
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
	return " until " + logtime.Format(resetsAt)
}

func withCause(summary string, cause error) string {
	if cause == nil {
		return summary
	}
	return summary + ": " + cause.Error()
}
