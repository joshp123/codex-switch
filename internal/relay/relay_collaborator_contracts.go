package relay

import (
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/routing"
	"github.com/joshp123/codex-switch/internal/store"
)

// TaskRouter keeps transport code independent of pin selection and movement.
type TaskRouter interface {
	RouteForModelCatalogue() (routing.RouteDecision, error)
	RouteForTask(id store.TaskID, kind store.WorkKind, modelSlug string) (routing.RouteDecision, error)
	RouteForProviderTask(store.TaskID, store.WorkKind, string, accounts.Provider, store.TaskID) (routing.RouteDecision, error)
	ReportUnsupportedModel(id store.TaskID, served store.AccountKey, modelSlug string) bool
	ReportExhaustedTurn(id store.TaskID, served store.AccountKey, resetsAt time.Time) string
	ReportUnauthorizedTurn(served store.AccountKey, rejected accounts.Credentials)
	ClaudePoolCapacity(modelSlug string) (accountFree bool, availableAgainAt time.Time, limitClaim string, err error)
}

// CompletionLedger is the only response fact the relay persists.
type CompletionLedger interface {
	RecordCompletedResponse(store.CompletedResponseRecord) error
}

type ModelRequestCounters interface {
	RecordModelRequestOutcome(store.AccountKey, time.Time, store.ModelRequestOutcome) error
}
