package routing

import (
	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

// AccountRoutingFactsSource isolates selection from account-home discovery.
type AccountRoutingFactsSource interface {
	ReadAccountRoutingFacts() ([]accounts.AccountRoutingFact, error)
	RequestAccountRoutingFactsRefresh()
	// ReportCodexWorkWaiting says Codex model work is waiting for an account,
	// which may justify spending a reset credit.
	ReportCodexWorkWaiting()
	// ResetCreditComesFirst says a reset credit is about to be spent, so no
	// account may draw its usage credits yet.
	ResetCreditComesFirst() bool
	ExcludeUnsupportedAccountModel(accountKey store.AccountKey, modelSlug string)
	RefreshAccountCredentialsAfterUnauthorized(accountKey store.AccountKey, rejected accounts.Credentials) error
}
