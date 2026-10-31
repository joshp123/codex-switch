package routing

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strings"
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
	AccountName store.AccountName
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
}

// CreateTaskRouter keeps allowance refresh ownership outside routing's single selection rule.
func CreateTaskRouter(storeDatabase *store.StoreDatabase, accountFacts AccountRoutingFactsSource) *TaskRouter {
	return &TaskRouter{storeDatabase: storeDatabase, accountFacts: accountFacts, currentTime: time.Now}
}

// RouteForTask keeps model work on its task's account while that account is
// available and gives new work the account with the most weekly allowance left.
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
		fact, inPool := factForAccount(facts, pin.AccountName)
		if !inPool {
			pinnedAccountProblem = "no longer in the account pool"
		} else if pinnedAccountProblem, err = router.accountUnavailableReason(fact, modelSlug); err != nil {
			return RouteDecision{}, err
		} else if provider == accounts.ProviderClaude && workKind == store.ModelTurnWorkKind &&
			router.currentTime().Sub(pin.PinnedAt) > claudePromptCacheLifetime {
			pinnedAccountProblem = "its prompt cache expired after an hour without requests"
		} else if pinnedAccountProblem == "" {
			if provider == accounts.ProviderClaude && workKind == store.ModelTurnWorkKind {
				// The pin's time is the session's last model request, so it
				// expires with the prompt cache.
				if _, err := router.storeDatabase.CompareAndSwapTaskAccountPin(taskID, pin.AccountName, pin.AccountName, router.currentTime().UTC()); err != nil {
					return RouteDecision{}, err
				}
			}
			return RouteDecision{AccountName: pin.AccountName, DisplayName: fact.DisplayName, Credentials: fact.Credentials}, nil
		}
	}

	if !pinned && taskID != "" && parentTaskID != "" && workKind == store.ModelTurnWorkKind {
		parentPin, parentPinned, err := router.storeDatabase.ReadTaskAccountPin(parentTaskID)
		if err != nil {
			return RouteDecision{}, err
		}
		if parentPinned {
			if parentFact, present := factForAccount(facts, parentPin.AccountName); present {
				problem, err := router.accountUnavailableReason(parentFact, modelSlug)
				if err != nil {
					return RouteDecision{}, err
				}
				if problem == "" && !accounts.SoftReserveReached(parentFact, router.currentTime().UTC()) {
					winner, err := router.storeDatabase.PinTaskAccountIfAbsent(store.TaskAccountPin{TaskID: taskID, AccountName: parentPin.AccountName, PinnedAt: router.currentTime().UTC()})
					if err != nil {
						return RouteDecision{}, err
					}
					if winner.AccountName == parentPin.AccountName {
						return RouteDecision{AccountName: parentPin.AccountName, DisplayName: parentFact.DisplayName, Credentials: parentFact.Credentials, Assignment: "fork keeps its parent's account " + parentFact.DisplayName}, nil
					}
					return router.RouteForProviderTask(taskID, workKind, modelSlug, provider, parentTaskID)
				}
			}
		}
	}
	selected, found, err := router.selectAccountWithMostWeeklyCapacity(facts, taskID, "", modelSlug)
	if err != nil {
		return RouteDecision{}, err
	}
	if !found {
		router.accountFacts.RequestAccountRoutingFactsRefresh()
		if provider == accounts.ProviderClaude {
			availableAgainAt, limitClaim := router.claudePoolAvailableAgain(facts, modelSlug)
			return RouteDecision{}, &ClaudePoolExhaustedError{AccountProblems: router.describeUnavailableAccounts(facts, modelSlug), AvailableAgainAt: availableAgainAt, LimitClaim: limitClaim}
		}
		return RouteDecision{}, fmt.Errorf("%w: %s", ErrNoAvailableAccount, router.describeUnavailableAccounts(facts, modelSlug))
	}

	decision := RouteDecision{AccountName: selected.AccountName, DisplayName: selected.DisplayName, Credentials: selected.Credentials}
	if workKind != store.ModelTurnWorkKind {
		return decision, nil
	}
	choice := router.describeChoice(selected, modelSlug)
	switch {
	case taskID == "":
		decision.Assignment = "served by " + choice + "; not pinned to a task"
		return decision, nil
	case pinned:
		moved, err := router.storeDatabase.CompareAndSwapTaskAccountPin(taskID, pin.AccountName, selected.AccountName, router.currentTime().UTC())
		if err != nil {
			return RouteDecision{}, err
		}
		if !moved {
			return router.RouteForProviderTask(taskID, workKind, modelSlug, provider, parentTaskID)
		}
		if selected.AccountName == pin.AccountName {
			decision.Assignment = fmt.Sprintf("stays on %s (%s)", choice, pinnedAccountProblem)
			return decision, nil
		}
		decision.Assignment = fmt.Sprintf("moved from %s (%s) to %s", router.accountDisplayName(pin.AccountName), pinnedAccountProblem, choice)
		return decision, nil
	}
	winner, err := router.storeDatabase.PinTaskAccountIfAbsent(store.TaskAccountPin{
		TaskID: taskID, AccountName: selected.AccountName, PinnedAt: router.currentTime().UTC(),
	})
	if err != nil {
		return RouteDecision{}, err
	}
	if winner.AccountName != selected.AccountName {
		return router.RouteForProviderTask(taskID, workKind, modelSlug, provider, parentTaskID)
	}
	decision.Assignment = "new task assigned to " + choice
	return decision, nil
}

// ReportExhaustedTurn changes the account for the next client request; routing never resends work.
// It returns what happened to the task, for the proxy log.
func (router *TaskRouter) ReportExhaustedTurn(taskID store.TaskID, servedAccountName store.AccountName, resetsAt time.Time) string {
	outcome, err := router.reportExhaustedTaskPin(taskID, servedAccountName, resetsAt)
	if err != nil {
		return fmt.Sprintf("%s hit its usage limit%s; could not move the task: %v", router.accountDisplayName(servedAccountName), resetSuffix(resetsAt), err)
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
		reason, err := router.accountUnavailableReason(fact, modelSlug)
		if err != nil {
			return false, time.Time{}, "", err
		}
		if reason == "" {
			return true, time.Time{}, "", nil
		}
	}
	availableAgainAt, limitClaim = router.claudePoolAvailableAgain(facts, modelSlug)
	return false, availableAgainAt, limitClaim, nil
}

// ReportUnauthorizedTurn refreshes rejected credentials for the client's next request without replaying the failed turn.
func (router *TaskRouter) ReportUnauthorizedTurn(servedAccountName store.AccountName, rejectedCredentials accounts.Credentials) {
	if err := router.accountFacts.RefreshAccountCredentialsAfterUnauthorized(servedAccountName, rejectedCredentials); err != nil {
		log.Printf("%s: the provider rejected its login and refreshing it failed: %v", router.accountDisplayName(servedAccountName), err)
	}
}

func (router *TaskRouter) reportExhaustedTaskPin(taskID store.TaskID, servedAccountName store.AccountName, resetsAt time.Time) (string, error) {
	limitReached := fmt.Sprintf("%s hit its usage limit%s", router.accountDisplayName(servedAccountName), resetSuffix(resetsAt))
	if err := router.storeDatabase.RecordAccountReportedReset(servedAccountName, resetsAt, router.currentTime().UTC()); err != nil {
		return "", err
	}
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return "", fmt.Errorf("read account routing facts: %w", err)
	}
	if served, found := factForAccount(facts, servedAccountName); found {
		facts = factsForProvider(facts, served.Provider)
	}
	selected, found, err := router.selectAccountWithMostWeeklyCapacity(facts, taskID, servedAccountName, "")
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
		taskID, servedAccountName, selected.AccountName, router.currentTime().UTC())
	if err != nil || !moved {
		return limitReached, err
	}
	return limitReached + "; task moved to " + router.describeChoice(selected, "") + " for its next request", nil
}

func (router *TaskRouter) selectAccountWithMostWeeklyCapacity(facts []accounts.AccountRoutingFact, taskID store.TaskID, excludedAccountName store.AccountName, modelSlug string) (accounts.AccountRoutingFact, bool, error) {
	var selected accounts.AccountRoutingFact
	selectedCapacity := -1.0
	selectedSoftReserveReached := false
	for _, fact := range facts {
		if fact.AccountName == excludedAccountName {
			continue
		}
		problem, err := router.accountUnavailableReason(fact, modelSlug)
		if err != nil {
			return accounts.AccountRoutingFact{}, false, err
		}
		if problem != "" {
			continue
		}
		capacity, known := router.weeklyAllowanceLeft(fact, modelSlug)
		if !known {
			capacity = -0.5
		}
		softReserveReached := accounts.SoftReserveReached(fact, router.currentTime().UTC())
		moreCapacity := selectedCapacity < capacity || (selectedCapacity == capacity && taskID != "" && accountWinsEqualCapacityTie(taskID, fact.AccountName, selected.AccountName))
		if selected.AccountName == "" || (selectedSoftReserveReached && !softReserveReached) || (selectedSoftReserveReached == softReserveReached && moreCapacity) {
			selected = fact
			selectedCapacity = capacity
			selectedSoftReserveReached = softReserveReached
		}
	}
	return selected, selected.AccountName != "", nil
}

// describeChoice names the selected account and its reported weekly allowance.
func (router *TaskRouter) describeChoice(selected accounts.AccountRoutingFact, modelSlug string) string {
	weeklyFreePercent, reported := router.weeklyAllowanceLeft(selected, modelSlug)
	if !reported {
		return fmt.Sprintf("%s, which reports no weekly allowance", selected.DisplayName)
	}
	return fmt.Sprintf("%s, with %.1f%% weekly allowance left", selected.DisplayName, weeklyFreePercent)
}

func (router *TaskRouter) describeUnavailableAccounts(facts []accounts.AccountRoutingFact, modelSlug string) string {
	if len(facts) == 0 {
		return "no account has been read yet"
	}
	problems := make([]string, 0, len(facts))
	for _, fact := range facts {
		problem, err := router.accountUnavailableReason(fact, modelSlug)
		if err != nil {
			problem = err.Error()
		}
		problems = append(problems, fact.DisplayName+" "+problem)
	}
	return strings.Join(problems, "; ")
}

func accountWinsEqualCapacityTie(taskID store.TaskID, candidateAccountName, selectedAccountName store.AccountName) bool {
	candidateRank := sha256.Sum256([]byte(string(taskID) + "\x00" + string(candidateAccountName)))
	selectedRank := sha256.Sum256([]byte(string(taskID) + "\x00" + string(selectedAccountName)))
	return bytes.Compare(candidateRank[:], selectedRank[:]) > 0
}

// ReportUnsupportedModel prepares the next client request without resending rejected work.
func (router *TaskRouter) ReportUnsupportedModel(taskID store.TaskID, accountName store.AccountName, modelSlug string) bool {
	router.accountFacts.ExcludeUnsupportedAccountModel(accountName, modelSlug)
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		log.Printf("read model routing facts: %v", err)
		return false
	}
	if served, found := factForAccount(facts, accountName); found {
		facts = factsForProvider(facts, served.Provider)
	}
	selected, found, err := router.selectAccountWithMostWeeklyCapacity(accountsWithKnownModels(facts), taskID, accountName, modelSlug)
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
			if fact.AccountName == accountName {
				rejectedAccountExcluded = fact.SupportedModelSlugs != nil && !accountSupportsModel(fact, modelSlug)
			} else if accountCredentialsUsable(fact.Credentials) && fact.SupportedModelSlugs != nil && accountSupportsModel(fact, modelSlug) {
				supportedAccountExists = true
			}
		}
		return rejectedAccountExcluded && supportedAccountExists
	}
	moved, err := router.storeDatabase.CompareAndSwapTaskAccountPin(taskID, accountName, selected.AccountName, router.currentTime().UTC())
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
	return RouteDecision{AccountName: selected.AccountName, DisplayName: selected.DisplayName, Credentials: selected.Credentials}, nil
}

// ReadAccountAvailability uses the same rule as admission. An empty reason means
// the account can take new work for modelSlug.
func (router *TaskRouter) ReadAccountAvailability(modelSlug string) ([]accounts.AccountRoutingFact, map[store.AccountName]string, error) {
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return nil, nil, err
	}
	unavailableReasons := make(map[store.AccountName]string, len(facts))
	for _, fact := range facts {
		if unavailableReasons[fact.AccountName], err = router.accountUnavailableReason(fact, availabilityModel(fact, modelSlug)); err != nil {
			return nil, nil, err
		}
	}
	return facts, unavailableReasons, nil
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

func (router *TaskRouter) accountDisplayName(key store.AccountName) string {
	facts, err := router.accountFacts.ReadAccountRoutingFacts()
	if err != nil {
		return "account"
	}
	if fact, found := factForAccount(facts, key); found {
		return fact.DisplayName
	}
	return "removed account"
}
