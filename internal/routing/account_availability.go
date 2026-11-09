package routing

import (
	"fmt"
	"log"
	"math"
	"slices"
	"strconv"
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
	// DrawsCredits means the account can take the work although its included
	// allowance is used up, because OpenAI draws its usage credits for it.
	DrawsCredits bool
}

// usageCreditFloor stops new work; in-flight work can finish below this balance.
const usageCreditFloor = 2000

// LogText is the reason as the proxy log writes it.
func (unavailability AccountUnavailability) LogText() string {
	return unavailability.Reason + resetSuffix(unavailability.Until)
}

// accountUnavailability is the one rule for whether an account may take new
// model work for modelSlug. It logs when an account work was sent to while it
// drew its usage credits may no longer draw them.
func (router *TaskRouter) accountUnavailability(fact accounts.AccountRoutingFact, modelSlug string) (AccountUnavailability, error) {
	unavailability, credits, err := router.allowanceRule(fact, modelSlug, true)
	if err == nil && fact.Provider != accounts.ProviderClaude {
		router.logCreditDrawStop(fact, credits)
	}
	return unavailability, err
}

// creditDraw is whether an account's allowance lets it draw its usage
// credits, apart from whether it offers the model, and why not.
type creditDraw struct {
	draws         bool
	whyNotDrawing string
}

// allowanceRule is accountUnavailability. With creditsAllowed false, an
// account whose included allowance is used up never draws its usage credits.
func (router *TaskRouter) allowanceRule(fact accounts.AccountRoutingFact, modelSlug string, creditsAllowed bool) (AccountUnavailability, creditDraw, error) {
	if problem := accountAccessProblem(fact); problem.Reason != "" {
		return problem, creditDraw{whyNotDrawing: problem.Reason}, nil
	}
	now := router.currentTime().UTC()
	if fact.CreditBalanceBlockedUntil.After(now) {
		reason := "usage credit balance fell below 1,000"
		return AccountUnavailability{Reason: reason, Until: fact.CreditBalanceBlockedUntil}, creditDraw{whyNotDrawing: reason}, nil
	}
	if reason := accounts.ReserveUnavailableReason(fact, now); reason != "" {
		return AccountUnavailability{Reason: reason}, creditDraw{whyNotDrawing: reason}, nil
	}
	if fact.Provider == accounts.ProviderClaude {
		reason, resetsAt := accounts.ClaudeUnavailableReason(fact, modelSlug, now)
		return AccountUnavailability{Reason: reason, Until: resetsAt}, creditDraw{}, nil
	}
	if !fact.AllowanceSample.ObservedAt.IsZero() {
		// OpenAI reports an account allowed while it draws usage credits, so
		// only the weekly window says whether included allowance is left.
		if _, known := accounts.AnalyseAllowance(fact.AllowanceSample, fact.AccountState, now, accounts.Weekly); !known {
			reason := "OpenAI's allowance reading gives no current weekly allowance"
			return AccountUnavailability{Reason: reason}, creditDraw{whyNotDrawing: reason}, nil
		}
	}
	credits := creditDraw{whyNotDrawing: "its included allowance is available again"}
	for _, kind := range []accounts.AllowanceWindowKind{accounts.FiveHours, accounts.Weekly} {
		window, known := accounts.AnalyseAllowance(fact.AllowanceSample, fact.AccountState, now, kind)
		if !known || window.FreePercent > 0 {
			continue
		}
		usedUp := AccountUnavailability{Reason: fmt.Sprintf("%s allowance used up", kind), Until: window.ResetsAt}
		if !creditsAllowed {
			return usedUp, creditDraw{whyNotDrawing: usedUp.Reason}, nil
		}
		problem, err := router.creditDrawProblem(fact, modelSlug)
		if err != nil || problem != "" {
			return usedUp, creditDraw{whyNotDrawing: problem}, err
		}
		credits = creditDraw{draws: true}
		break
	}
	if fact.AccountState == accounts.AccountReady && fact.AllowanceSample.Allowed && !fact.AllowanceSample.LimitReached {
		// A provider read begun before exhaustion cannot supersede that exhaustion.
		cleared, err := router.storeDatabase.ClearAccountExhaustionBefore(fact.AccountKey, fact.RefreshedAt)
		if err != nil {
			return AccountUnavailability{}, credits, err
		}
		if cleared {
			log.Printf("%s can take new work again: OpenAI's allowance reading at %s shows it is no longer limited",
				fact.DisplayName, logtime.Format(fact.AllowanceSample.ObservedAt))
		}
	}
	resetsAt, exhausted, err := router.storeDatabase.ReadAccountReportedReset(fact.AccountKey)
	if err != nil {
		return AccountUnavailability{}, credits, err
	}
	if exhausted {
		return AccountUnavailability{Reason: "hit its usage limit", Until: resetsAt}, creditDraw{whyNotDrawing: "it hit its usage limit"}, nil
	}
	if modelSlug != "" && fact.SupportedModelSlugs == nil {
		return AccountUnavailability{Reason: "its model list has not been read yet"}, credits, nil
	}
	if !accountSupportsModel(fact, modelSlug) {
		return AccountUnavailability{Reason: "does not offer " + modelSlug}, credits, nil
	}
	return AccountUnavailability{DrawsCredits: credits.draws}, credits, nil
}

// creditDrawProblem says why a Codex account whose included allowance is
// used up may not draw its usage credits, or "" when it may. Credits are the
// last resort: while another account can take the work on included
// allowance, or a reset credit is about to be spent, the work goes there or
// waits. New work stops when the observed balance reaches usageCreditFloor.
func (router *TaskRouter) creditDrawProblem(fact accounts.AccountRoutingFact, modelSlug string) (string, error) {
	balance := fact.AllowanceSample.CreditBalance
	if balance == nil {
		return "OpenAI reports no usage credits for it", nil
	}
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return "", fmt.Errorf("read account routing facts: %w", err)
	}
	for _, other := range factsForProvider(facts, fact.Provider) {
		if other.AccountKey == fact.AccountKey {
			continue
		}
		unavailability, _, err := router.allowanceRule(other, modelSlug, false)
		if err != nil {
			return "", err
		}
		if unavailability.Reason == "" {
			return other.DisplayName + " can take the work on its included allowance", nil
		}
	}
	if router.accountFacts.ResetCreditComesFirst() {
		return "a reset credit is about to be spent, and reset credits expire sooner", nil
	}
	if *balance <= usageCreditFloor {
		return fmt.Sprintf("the balance reached the %s cutoff", formatCredits(usageCreditFloor)), nil
	}
	return "", nil
}

// logCreditDrawStop logs when an account that work was sent to while it drew
// its usage credits may no longer draw them.
func (router *TaskRouter) logCreditDrawStop(fact accounts.AccountRoutingFact, credits creditDraw) {
	if credits.draws {
		return
	}
	router.creditDrawMutex.Lock()
	drew := router.drawingCredits[fact.AccountKey]
	delete(router.drawingCredits, fact.AccountKey)
	router.creditDrawMutex.Unlock()
	if !drew {
		return
	}
	at := ""
	if balance := fact.AllowanceSample.CreditBalance; balance != nil {
		at = " at " + formatCredits(*balance)
	}
	log.Printf("%s stops drawing its usage credits%s: %s.", fact.DisplayName, at, credits.whyNotDrawing)
}

// noteCreditDrawStart logs when model work first goes to an account that
// draws its usage credits for it.
func (router *TaskRouter) noteCreditDrawStart(accountKey store.AccountKey, modelSlug string) error {
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return err
	}
	fact, found := factForAccount(facts, accountKey)
	if !found || fact.Provider == accounts.ProviderClaude {
		return nil
	}
	_, credits, err := router.allowanceRule(fact, modelSlug, true)
	if err != nil || !credits.draws {
		return err
	}
	router.creditDrawMutex.Lock()
	drew := router.drawingCredits[accountKey]
	router.drawingCredits[accountKey] = true
	router.creditDrawMutex.Unlock()
	if !drew {
		log.Printf("%s starts drawing its usage credits at %s: no account has included allowance left and no reset credit is about to be spent. New work stops at %s.",
			fact.DisplayName, formatCredits(*fact.AllowanceSample.CreditBalance), formatCredits(usageCreditFloor))
	}
	return nil
}

// formatCredits writes a credit amount as OpenAI's Codex client does: whole
// credits, here with thousands separated.
func formatCredits(credits float64) string {
	whole := strconv.FormatFloat(math.Round(credits), 'f', 0, 64)
	sign := ""
	if strings.HasPrefix(whole, "-") {
		sign, whole = "-", whole[1:]
	}
	for index := len(whole) - 3; index > 0; index -= 3 {
		whole = whole[:index] + "," + whole[index:]
	}
	return sign + whole
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
