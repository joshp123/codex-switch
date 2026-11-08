package usage

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/joshp123/codex-switch/internal/menupb"
	"github.com/joshp123/codex-switch/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func routedWorkWithRequestCounts(state *usageFold, metadata taskMetadataLookup, accountKeys []string, now, requestsStartedAt time.Time, counts []store.ModelRequestCounts) (*menupb.ProxyActivity, error) {
	activity := &menupb.ProxyActivity{StartedAt: timestamppb.New(state.today), EndedAt: timestamppb.New(now)}
	accountActivity := make(map[string]*menupb.ProxyAccountActivity)
	for _, key := range accountKeys {
		accountActivity[key] = &menupb.ProxyAccountActivity{AccountKey: key}
	}
	if !requestsStartedAt.IsZero() {
		activity.RequestsStartedAt = timestamppb.New(requestsStartedAt)
	}
	for _, count := range counts {
		key := string(count.AccountKey)
		if accountActivity[key] == nil {
			accountActivity[key] = &menupb.ProxyAccountActivity{AccountKey: key}
		}
		accountActivity[key].RequestCounts = &menupb.ModelRequestCounts{Requests: count.Requests, Successful: count.Successful, Failed: count.Failed, Interrupted: count.Interrupted}
	}
	for key, amount := range state.proxyActivity {
		account := string(key.Account)
		if accountActivity[account] == nil {
			accountActivity[account] = &menupb.ProxyAccountActivity{AccountKey: account}
		}
		accountActivity[account].CompletedResponses += amount.Count
		accountActivity[account].Tokens += amount.Tokens
	}
	for _, account := range accountActivity {
		activity.Accounts = append(activity.Accounts, account)
	}
	slices.SortFunc(activity.Accounts, func(a, b *menupb.ProxyAccountActivity) int { return cmp.Compare(a.AccountKey, b.AccountKey) })
	// Activity contains only proxy rows, so its tasks do not use native turn owners.
	tasks, err := totalTaskCohorts(maps.All(state.proxyActivity), metadata, nil, state.today, now)
	if err != nil {
		return nil, err
	}
	for _, total := range tasks {
		if len(activity.Tasks) == 10 {
			break
		}
		activity.Tasks = append(activity.Tasks, completedTask(total))
	}
	return activity, nil
}
