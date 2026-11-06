package routing

import (
	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

// Metadata has no inference allowance. All native routes reach the provider,
// including routes introduced by a newer client.
func (router *TaskRouter) routeClaudeMetadata(facts []accounts.AccountRoutingFact, taskID store.TaskID) (RouteDecision, error) {
	if taskID != "" {
		pin, pinned, err := router.storeDatabase.ReadTaskAccountPin(taskID)
		if err != nil {
			return RouteDecision{}, err
		}
		if pinned {
			if fact, present := factForAccount(facts, pin.AccountKey); present && accountCredentialsUsable(fact.Credentials) && fact.AccountState != accounts.AccountSignInRequired {
				return RouteDecision{AccountKey: fact.AccountKey, DisplayName: fact.DisplayName, Credentials: fact.Credentials}, nil
			}
		}
	}
	for _, fact := range facts {
		if accountCredentialsUsable(fact.Credentials) && fact.AccountState != accounts.AccountSignInRequired {
			return RouteDecision{AccountKey: fact.AccountKey, DisplayName: fact.DisplayName, Credentials: fact.Credentials}, nil
		}
	}
	router.accountFacts.RequestAccountRoutingFactsRefresh()
	return RouteDecision{}, ErrNoAvailableAccount
}
