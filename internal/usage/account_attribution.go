package usage

import (
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

type allowanceIdentityKey struct{ seconds, reset int64 }

// attributeCompletion prefers the serving account and otherwise uses only
// authenticated allowance windows covering the completion's timestamp.
// attributeCompletion also notes in consulted every allowance window it
// looked up, so a later change of that window's owner rebuilds the totals.
func attributeCompletion(completion *reconciledCompletion, allowance *sessionAllowance, providerByKey map[store.AccountKey]store.ProviderAccountID, owners map[allowanceIdentityKey]store.ProviderAccountID, consulted map[allowanceIdentityKey]bool) {
	completion.ProviderAccountID = providerByKey[completion.AccountKey]
	if completion.ProviderAccountID != "" {
		return
	}
	if allowance == nil || (allowance.LimitID != "" && allowance.LimitID != "codex") {
		return
	}
	for _, window := range []*sessionAllowanceWindow{allowance.Primary, allowance.Secondary} {
		if window == nil || window.WindowMinutes <= 0 || window.ResetsAt.IsZero() {
			continue
		}
		duration := window.WindowMinutes * 60
		if completion.At.Before(window.ResetsAt.Add(-time.Duration(duration)*time.Second)) || !completion.At.Before(window.ResetsAt) {
			continue
		}
		key := allowanceIdentityKey{duration, window.ResetsAt.Unix()}
		consulted[key] = true
		if owner := owners[key]; owner != "" {
			completion.ProviderAccountID = owner
		}
	}
}
