package routing

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

// ErrNoAvailableAccount means new work must wait for account capacity.
var ErrNoAvailableAccount = errors.New("no account can take this work")

// ClaudePoolExhaustedError is ErrNoAvailableAccount for Claude model work. It
// says when an allowance reset next frees an account; AvailableAgainAt is zero
// when no reset will, for example when every account is signed out.
type ClaudePoolExhaustedError struct {
	AccountProblems  string
	AvailableAgainAt time.Time
	// LimitClaim is Anthropic's name for the limit that ends at AvailableAgainAt,
	// such as five_hour or seven_day.
	LimitClaim string
}

func (exhausted *ClaudePoolExhaustedError) Error() string {
	return ErrNoAvailableAccount.Error() + ": " + exhausted.AccountProblems
}

func (exhausted *ClaudePoolExhaustedError) Unwrap() error { return ErrNoAvailableAccount }

// RouteDecision carries credentials with the account identity so the relay cannot mix accounts.
type RouteDecision struct {
	AccountKey  store.AccountKey
	DisplayName string
	Credentials accounts.Credentials
	// Assignment is empty when model work stays on its pinned account. Otherwise it
	// says, in product terms, which account now serves the work and why; the relay
	// logs it with the client that sent the request.
	Assignment string
}

// TaskRouter reads every pin decision from SQLite so external repairs are visible without restart.
type TaskRouter struct {
	storeDatabase *store.StoreDatabase
	accountFacts  AccountRoutingFactsSource
	currentTime   func() time.Time

	// drawingCredits holds each account model work went to while it drew its
	// usage credits, until it may no longer draw them, so the log says once
	// when an account starts and stops.
	creditDrawMutex sync.Mutex
	drawingCredits  map[store.AccountKey]bool
}

// CreateTaskRouter keeps allowance refresh ownership outside routing's single selection rule.
func CreateTaskRouter(storeDatabase *store.StoreDatabase, accountFacts AccountRoutingFactsSource) *TaskRouter {
	return &TaskRouter{storeDatabase: storeDatabase, accountFacts: accountFacts, currentTime: time.Now, drawingCredits: map[store.AccountKey]bool{}}
}

// RouteForTask keeps model work on its task's account while that account is
// available and gives new work the account selectAccountForNewWork ranks first.
// Work without a task ID gets the same choice as a new task but is never pinned.
// Metadata requests never create or move a pin.
func (router *TaskRouter) RouteForTask(taskID store.TaskID, workKind store.WorkKind, modelSlug string) (RouteDecision, error) {
	return router.RouteForProviderTask(taskID, workKind, modelSlug, accounts.ProviderChatGPT, "")
}

// claudePromptCacheLifetime is how long Claude Code's subscription prompt cache
// lasts. A session idle for longer has nothing warm to keep on its account.
const claudePromptCacheLifetime = time.Hour

// RouteForProviderTask uses the same placement rule for each provider. A new fork
// first inherits its parent account when that account can serve this model.
func (router *TaskRouter) RouteForProviderTask(taskID store.TaskID, workKind store.WorkKind, modelSlug string, provider accounts.Provider, parentTaskID store.TaskID) (RouteDecision, error) {
	decision, err := router.routeForProviderTask(taskID, workKind, modelSlug, provider, parentTaskID)
	if err == nil && workKind == store.ModelTurnWorkKind {
		err = router.noteCreditDrawStart(decision.AccountKey, modelSlug)
	}
	return decision, err
}

func (router *TaskRouter) routeForProviderTask(taskID store.TaskID, workKind store.WorkKind, modelSlug string, provider accounts.Provider, parentTaskID store.TaskID) (RouteDecision, error) {
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return RouteDecision{}, fmt.Errorf("read account routing facts: %w", err)
	}
	facts = factsForProvider(facts, provider)
	if provider == accounts.ProviderClaude && workKind != store.ModelTurnWorkKind {
		return router.routeClaudeMetadata(facts, taskID)
	}
	var pin store.TaskAccountPin
	pinned := false
	if taskID != "" {
		if pin, pinned, err = router.storeDatabase.ReadTaskAccountPin(taskID); err != nil {
			return RouteDecision{}, err
		}
	}
	pinnedAccountProblem := ""
	if pinned {
		fact, inPool := factForAccount(facts, pin.AccountKey)
		var unavailability AccountUnavailability
		if !inPool {
			pinnedAccountProblem = "no longer in the account pool"
		} else if unavailability, err = router.accountUnavailability(fact, modelSlug); err != nil {
			return RouteDecision{}, err
		} else if provider == accounts.ProviderClaude && workKind == store.ModelTurnWorkKind &&
			router.currentTime().Sub(pin.PinnedAt) > claudePromptCacheLifetime {
			pinnedAccountProblem = "its prompt cache expired after an hour without requests"
		} else if pinnedAccountProblem = unavailability.LogText(); pinnedAccountProblem == "" {
			if provider == accounts.ProviderClaude && workKind == store.ModelTurnWorkKind {
				// The pin's time is the session's last model request, so it
				// expires with the prompt cache.
				if _, err := router.storeDatabase.CompareAndSwapTaskAccountPin(taskID, pin.AccountKey, pin.AccountKey, router.currentTime().UTC()); err != nil {
					return RouteDecision{}, err
				}
			}
			return RouteDecision{AccountKey: pin.AccountKey, DisplayName: fact.DisplayName, Credentials: fact.Credentials}, nil
		}
	}

	if !pinned && taskID != "" && parentTaskID != "" && workKind == store.ModelTurnWorkKind {
		parentPin, parentPinned, err := router.storeDatabase.ReadTaskAccountPin(parentTaskID)
		if err != nil {
			return RouteDecision{}, err
		}
		if parentPinned {
			if parentFact, present := factForAccount(facts, parentPin.AccountKey); present {
				problem, err := router.accountUnavailability(parentFact, modelSlug)
				if err != nil {
					return RouteDecision{}, err
				}
				if problem.Reason == "" && !accounts.SoftReserveReached(parentFact, router.currentTime().UTC()) {
					winner, err := router.storeDatabase.PinTaskAccountIfAbsent(store.TaskAccountPin{TaskID: taskID, AccountKey: parentPin.AccountKey, PinnedAt: router.currentTime().UTC()})
					if err != nil {
						return RouteDecision{}, err
					}
					if winner.AccountKey == parentPin.AccountKey {
						return RouteDecision{AccountKey: parentPin.AccountKey, DisplayName: parentFact.DisplayName, Credentials: parentFact.Credentials, Assignment: "fork keeps its parent's account " + parentFact.DisplayName}, nil
					}
					return router.RouteForProviderTask(taskID, workKind, modelSlug, provider, parentTaskID)
				}
			}
		}
	}
	selected, found, err := router.selectAccountForNewWork(facts, "", modelSlug)
	if err != nil {
		return RouteDecision{}, err
	}
	if !found {
		router.accountFacts.RequestAccountRoutingFactsRefresh()
		if provider == accounts.ProviderChatGPT && workKind == store.ModelTurnWorkKind {
			router.accountFacts.ReportCodexWorkWaiting()
		}
		if provider == accounts.ProviderClaude {
			availableAgainAt, limitClaim := router.claudePoolAvailableAgain(facts, modelSlug)
			return RouteDecision{}, &ClaudePoolExhaustedError{AccountProblems: router.describeUnavailableAccounts(facts, modelSlug), AvailableAgainAt: availableAgainAt, LimitClaim: limitClaim}
		}
		return RouteDecision{}, fmt.Errorf("%w: %s", ErrNoAvailableAccount, router.describeUnavailableAccounts(facts, modelSlug))
	}

	decision := RouteDecision{AccountKey: selected.AccountKey, DisplayName: selected.DisplayName, Credentials: selected.Credentials}
	if workKind != store.ModelTurnWorkKind {
		return decision, nil
	}
	choice := router.describeChoice(selected, modelSlug)
	switch {
	case taskID == "":
		decision.Assignment = "served by " + choice + "; not pinned to a task"
		return decision, nil
	case pinned:
		moved, err := router.storeDatabase.CompareAndSwapTaskAccountPin(taskID, pin.AccountKey, selected.AccountKey, router.currentTime().UTC())
		if err != nil {
			return RouteDecision{}, err
		}
		if !moved {
			return router.RouteForProviderTask(taskID, workKind, modelSlug, provider, parentTaskID)
		}
		if selected.AccountKey == pin.AccountKey {
			decision.Assignment = fmt.Sprintf("stays on %s (%s)", choice, pinnedAccountProblem)
			return decision, nil
		}
		decision.Assignment = fmt.Sprintf("moved from %s (%s) to %s", router.accountDisplayName(pin.AccountKey), pinnedAccountProblem, choice)
		return decision, nil
	}
	winner, err := router.storeDatabase.PinTaskAccountIfAbsent(store.TaskAccountPin{
		TaskID: taskID, AccountKey: selected.AccountKey, PinnedAt: router.currentTime().UTC(),
	})
	if err != nil {
		return RouteDecision{}, err
	}
	if winner.AccountKey != selected.AccountKey {
		return router.RouteForProviderTask(taskID, workKind, modelSlug, provider, parentTaskID)
	}
	decision.Assignment = "new task assigned to " + choice
	return decision, nil
}

// ReportExhaustedTurn changes the account for the next client request; routing never resends work.
// It returns what happened to the task, for the proxy log.
func (router *TaskRouter) ReportExhaustedTurn(taskID store.TaskID, servedAccountKey store.AccountKey, resetsAt time.Time) string {
	outcome, err := router.reportExhaustedTaskPin(taskID, servedAccountKey, resetsAt)
	if err != nil {
		return fmt.Sprintf("%s hit its usage limit%s; could not move the task: %v", router.accountDisplayName(servedAccountKey), resetSuffix(resetsAt), err)
	}
	return outcome
}

// ClaudePoolCapacity says whether a Claude account can take modelSlug now. If
// none can, it says when an allowance reset next frees one, and Anthropic's
// name for the limit that ends then.
func (router *TaskRouter) ClaudePoolCapacity(modelSlug string) (accountFree bool, availableAgainAt time.Time, limitClaim string, err error) {
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return false, time.Time{}, "", fmt.Errorf("read account routing facts: %w", err)
	}
	facts = factsForProvider(facts, accounts.ProviderClaude)
	for _, fact := range facts {
		unavailability, err := router.accountUnavailability(fact, modelSlug)
		if err != nil {
			return false, time.Time{}, "", err
		}
		if unavailability.Reason == "" {
			return true, time.Time{}, "", nil
		}
	}
	availableAgainAt, limitClaim = router.claudePoolAvailableAgain(facts, modelSlug)
	return false, availableAgainAt, limitClaim, nil
}

// ReportUnauthorizedTurn refreshes rejected credentials for the client's next request without replaying the failed turn.
func (router *TaskRouter) ReportUnauthorizedTurn(servedAccountKey store.AccountKey, rejectedCredentials accounts.Credentials) {
	if err := router.accountFacts.RefreshAccountCredentialsAfterUnauthorized(servedAccountKey, rejectedCredentials); err != nil {
		log.Printf("%s: the provider rejected its login and refreshing it failed: %v", router.accountDisplayName(servedAccountKey), err)
	}
}

func (router *TaskRouter) reportExhaustedTaskPin(taskID store.TaskID, servedAccountKey store.AccountKey, resetsAt time.Time) (string, error) {
	limitReached := fmt.Sprintf("%s hit its usage limit%s", router.accountDisplayName(servedAccountKey), resetSuffix(resetsAt))
	if err := router.storeDatabase.RecordAccountReportedReset(servedAccountKey, resetsAt, router.currentTime().UTC()); err != nil {
		return "", err
	}
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return "", fmt.Errorf("read account routing facts: %w", err)
	}
	if served, found := factForAccount(facts, servedAccountKey); found {
		facts = factsForProvider(facts, served.Provider)
	}
	selected, found, err := router.selectAccountForNewWork(facts, servedAccountKey, "")
	if err != nil {
		return "", err
	}
	router.accountFacts.RequestAccountRoutingFactsRefresh()
	if !found {
		return limitReached + "; no other account is available, so the next request waits for one", nil
	}
	if taskID == "" {
		return limitReached + "; the next request goes to " + router.describeChoice(selected, ""), nil
	}
	moved, err := router.storeDatabase.CompareAndSwapTaskAccountPin(
		taskID, servedAccountKey, selected.AccountKey, router.currentTime().UTC())
	if err != nil || !moved {
		return limitReached, err
	}
	return limitReached + "; task moved to " + router.describeChoice(selected, "") + " for its next request", nil
}

// selectAccountForNewWork ranks the accounts that can take new work. Accounts
// that draw usage credits come last, the largest balance first. Of the rest,
// accounts that have not reached their soft reserve come first. Then an OpenAI account
// whose week has not started, so that its weekly clock runs and its allowance
// refills a week later. Then the weekly allowance that resets soonest, because
// allowance left at a reset is lost and allowance that resets later keeps.
// Equal resets go to the account with more left.
func (router *TaskRouter) selectAccountForNewWork(facts []accounts.AccountRoutingFact, excludedAccountKey store.AccountKey, modelSlug string) (accounts.AccountRoutingFact, bool, error) {
	var selected accounts.AccountRoutingFact
	var selectedRank newWorkRank
	for _, fact := range facts {
		if fact.AccountKey == excludedAccountKey {
			continue
		}
		problem, err := router.accountUnavailability(fact, modelSlug)
		if err != nil {
			return accounts.AccountRoutingFact{}, false, err
		}
		if problem.Reason != "" {
			continue
		}
		weekly, weeklyKnown := router.weeklyAllowance(fact, modelSlug)
		rank := newWorkRank{softReserveReached: accounts.SoftReserveReached(fact, router.currentTime().UTC()), weekly: weekly, weeklyKnown: weeklyKnown, drawsCredits: problem.DrawsCredits}
		if rank.drawsCredits {
			rank.creditBalance = *fact.AllowanceSample.CreditBalance
		}
		if selected.AccountKey == "" || rank.before(selectedRank) {
			selected, selectedRank = fact, rank
		}
	}
	return selected, selected.AccountKey != "", nil
}

type newWorkRank struct {
	softReserveReached bool
	weekly             accounts.AllowanceWindow
	weeklyKnown        bool
	drawsCredits       bool
	creditBalance      float64
}

func (rank newWorkRank) before(other newWorkRank) bool {
	switch {
	case rank.drawsCredits != other.drawsCredits:
		return other.drawsCredits
	case rank.drawsCredits:
		return rank.creditBalance > other.creditBalance
	case rank.softReserveReached != other.softReserveReached:
		return other.softReserveReached
	case rank.weeklyKnown != other.weeklyKnown:
		return rank.weeklyKnown
	case rank.weekly.NotStarted != other.weekly.NotStarted:
		return rank.weekly.NotStarted
	case !rank.weekly.ResetsAt.Equal(other.weekly.ResetsAt):
		return rank.weekly.ResetsAt.Before(other.weekly.ResetsAt)
	}
	return rank.weekly.FreePercent > other.weekly.FreePercent
}

// describeChoice names the selected account and its reported weekly allowance.
func (router *TaskRouter) describeChoice(selected accounts.AccountRoutingFact, modelSlug string) string {
	weekly, reported := router.weeklyAllowance(selected, modelSlug)
	switch {
	case reported && weekly.FreePercent <= 0 && selected.AllowanceSample.CreditBalance != nil:
		return fmt.Sprintf("%s, drawing its usage credits, %s left", selected.DisplayName, formatCredits(*selected.AllowanceSample.CreditBalance))
	case !reported:
		return fmt.Sprintf("%s, which reports no weekly allowance", selected.DisplayName)
	case weekly.NotStarted:
		return fmt.Sprintf("%s, whose weekly allowance has not started", selected.DisplayName)
	}
	return fmt.Sprintf("%s, with %.1f%% weekly allowance left%s", selected.DisplayName, weekly.FreePercent, resetSuffix(weekly.ResetsAt))
}

func (router *TaskRouter) describeUnavailableAccounts(facts []accounts.AccountRoutingFact, modelSlug string) string {
	if len(facts) == 0 {
		return "no account has been read yet"
	}
	problems := make([]string, 0, len(facts))
	for _, fact := range facts {
		unavailability, err := router.accountUnavailability(fact, modelSlug)
		problem := unavailability.LogText()
		if err != nil {
			problem = err.Error()
		}
		problems = append(problems, fact.DisplayName+" "+problem)
	}
	return strings.Join(problems, "; ")
}

// ReportUnsupportedModel prepares the next client request without resending rejected work.
func (router *TaskRouter) ReportUnsupportedModel(taskID store.TaskID, accountKey store.AccountKey, modelSlug string) bool {
	router.accountFacts.ExcludeUnsupportedAccountModel(accountKey, modelSlug)
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		log.Printf("read model routing facts: %v", err)
		return false
	}
	if served, found := factForAccount(facts, accountKey); found {
		facts = factsForProvider(facts, served.Provider)
	}
	selected, found, err := router.selectAccountForNewWork(accountsWithKnownModels(facts), accountKey, modelSlug)
	if err != nil {
		log.Printf("select supported model account: %v", err)
		return false
	}
	if !found {
		// The next model-aware request will skip the rejected account and use the
		// existing capacity wait until a supported account becomes available.
		rejectedAccountExcluded := false
		supportedAccountExists := false
		for _, fact := range facts {
			if fact.AccountKey == accountKey {
				rejectedAccountExcluded = fact.SupportedModelSlugs != nil && !accountSupportsModel(fact, modelSlug)
			} else if accountCredentialsUsable(fact.Credentials) && fact.SupportedModelSlugs != nil && accountSupportsModel(fact, modelSlug) {
				supportedAccountExists = true
			}
		}
		return rejectedAccountExcluded && supportedAccountExists
	}
	moved, err := router.storeDatabase.CompareAndSwapTaskAccountPin(taskID, accountKey, selected.AccountKey, router.currentTime().UTC())
	if err != nil {
		log.Printf("move unsupported model task pin: %v", err)
		return false
	}
	return moved
}

func accountsWithKnownModels(facts []accounts.AccountRoutingFact) []accounts.AccountRoutingFact {
	known := make([]accounts.AccountRoutingFact, 0, len(facts))
	for _, fact := range facts {
		if fact.SupportedModelSlugs != nil {
			known = append(known, fact)
		}
	}
	return known
}

// RouteForModelCatalogue reads model metadata from the widest observed account catalogue.
// Inference allowance does not restrict this read, and task pins are untouched.
func (router *TaskRouter) RouteForModelCatalogue() (RouteDecision, error) {
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return RouteDecision{}, fmt.Errorf("read model catalogue routing facts: %w", err)
	}
	var selected *accounts.AccountRoutingFact
	for index := range facts {
		fact := &facts[index]
		if fact.Provider != accounts.ProviderChatGPT || !accountCredentialsUsable(fact.Credentials) || fact.SupportedModelSlugs == nil {
			continue
		}
		if selected == nil || len(fact.SupportedModelSlugs) > len(selected.SupportedModelSlugs) {
			selected = fact
		}
	}
	if selected == nil {
		router.accountFacts.RequestAccountRoutingFactsRefresh()
		return RouteDecision{}, ErrNoAvailableAccount
	}
	return RouteDecision{AccountKey: selected.AccountKey, DisplayName: selected.DisplayName, Credentials: selected.Credentials}, nil
}

// ReadAccountAvailability uses the same rule as admission.
func (router *TaskRouter) ReadAccountAvailability(modelSlug string) ([]accounts.AccountRoutingFact, map[store.AccountKey]AccountUnavailability, error) {
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return nil, nil, err
	}
	unavailability := make(map[store.AccountKey]AccountUnavailability, len(facts))
	for _, fact := range facts {
		if unavailability[fact.AccountKey], err = router.accountUnavailability(fact, availabilityModel(fact, modelSlug)); err != nil {
			return nil, nil, err
		}
	}
	return facts, unavailability, nil
}

func factsForProvider(facts []accounts.AccountRoutingFact, provider accounts.Provider) []accounts.AccountRoutingFact {
	selected := make([]accounts.AccountRoutingFact, 0, len(facts))
	for _, fact := range facts {
		if fact.Provider == provider {
			selected = append(selected, fact)
		}
	}
	return selected
}

func availabilityModel(fact accounts.AccountRoutingFact, codexModel string) string {
	if fact.Provider == accounts.ProviderClaude {
		return ""
	}
	return codexModel
}

func (router *TaskRouter) accountDisplayName(key store.AccountKey) string {
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return "account"
	}
	if fact, found := factForAccount(facts, key); found {
		return fact.DisplayName
	}
	return "removed account"
}
