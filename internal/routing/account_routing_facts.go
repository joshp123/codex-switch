package routing

import (
	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

// AccountRoutingFactsSource isolates selection from account-home discovery.
type AccountRoutingFactsSource interface {
	ReadAccountRoutingFacts() ([]accounts.AccountRoutingFact, error)
	RequestAccountRoutingFactsRefresh()
	ExcludeUnsupportedAccountModel(accountKey store.AccountKey, modelSlug string)
	RefreshAccountCredentialsAfterUnauthorized(accountKey store.AccountKey, rejected accounts.Credentials) error
}
