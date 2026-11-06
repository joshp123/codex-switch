package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/maphash"
	"iter"
	"log"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

type usageCohortKey struct {
	TaskID, RootTurnID string
	Account            store.AccountKey
	Date               time.Time
}

type usageCohortAmount struct {
	Tokens                             int64
	Cost                               nanoUSD
	UnknownPrices, Unattributed, Count int64
	LastCompletedAt                    time.Time
}

func (amount *usageCohortAmount) add(other usageCohortAmount) {
	amount.Tokens += other.Tokens
	amount.Cost += other.Cost
	amount.UnknownPrices += other.UnknownPrices
	amount.Unattributed += other.Unattributed
	amount.Count += other.Count
	if other.LastCompletedAt.After(amount.LastCompletedAt) {
		amount.LastCompletedAt = other.LastCompletedAt
	}
}

type pendingUsageContribution struct {
	Cohort usageCohortKey
	Amount usageCohortAmount
}

type responseCounted uint8

const (
	countedProxy responseCounted = 1 << iota
	countedNative
	countedNeedsAllowance
)

type usageTurnOwner struct {
	TaskID string
	At     int64
	Offset int64
}

type usageFold struct {
	marks                          usageRowWatermarks
	today, localDay, standardStart time.Time
	deferredUntil                  time.Time
	hashSeed                       maphash.Seed
	responses                      map[uint64]responseCounted
	pending                        map[uint64]pendingUsageContribution
	settled                        map[usageCohortKey]usageCohortAmount
	proxyActivity                  map[usageCohortKey]usageCohortAmount
	standardCost                   map[string]map[modelDay]nanoUSD
	metadata                       map[string]*taskMetadata
	childParents                   map[string]map[string]bool
	turnOwners                     map[string]usageTurnOwner
	firstProxy                     map[string]time.Time
	latestLegacy                   map[string]time.Time
	providerByKey                  map[store.AccountKey]store.ProviderAccountID
	keysByProvider                 map[store.ProviderAccountID]store.AccountKey
	allowanceOwners                map[allowanceIdentityKey]store.ProviderAccountID
	// consultedAllowanceWindows are the windows some counted usage was
	// attributed through; only a change to one of their owners matters.
	consultedAllowanceWindows map[allowanceIdentityKey]bool
}

type usageRefoldReason string

func (reason usageRefoldReason) Error() string { return string(reason) }

var lastUsageTime = time.Unix(0, math.MaxInt64)

func newUsageFold(today, localDay, standardStart time.Time, providers map[store.AccountKey]store.ProviderAccountID, keys map[store.ProviderAccountID]store.AccountKey, owners map[allowanceIdentityKey]store.ProviderAccountID) *usageFold {
	return &usageFold{
		today: today, localDay: localDay, standardStart: standardStart, hashSeed: maphash.MakeSeed(),
		responses: make(map[uint64]responseCounted), pending: make(map[uint64]pendingUsageContribution),
		settled: make(map[usageCohortKey]usageCohortAmount), proxyActivity: make(map[usageCohortKey]usageCohortAmount), standardCost: make(map[string]map[modelDay]nanoUSD),
		metadata: make(map[string]*taskMetadata), childParents: make(map[string]map[string]bool), turnOwners: make(map[string]usageTurnOwner), firstProxy: make(map[string]time.Time), latestLegacy: make(map[string]time.Time),
		providerByKey: providers, keysByProvider: keys, allowanceOwners: owners, consultedAllowanceWindows: make(map[allowanceIdentityKey]bool),
	}
}

func localDate(at time.Time) time.Time {
	local := at.Local()
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
}

func oldestCreditDate(credits map[string][]accounts.OpenAIModelDayCredits) (time.Time, error) {
	oldest := ""
	for _, days := range credits {
		for _, day := range days {
			if oldest == "" || day.Date < oldest {
				oldest = day.Date
			}
		}
	}
	if oldest == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.DateOnly, oldest)
}

// refreshUsageFold holds only aggregates, response hashes and the proxy work
// still waiting for its native line. Both startup and rare refolds use foldRows.
func (ledger *UsageLedger) refreshUsageFold(ctx context.Context, now time.Time, pool []accounts.PoolAccount, standardStart time.Time) (*usageFold, error) {
	transaction, err := ledger.sqlDatabase.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	marks, err := readUsageRowWatermarks(ctx, transaction)
	if err != nil {
		return nil, err
	}
	providers, keys, owners, err := readUsageIdentities(ctx, transaction, pool)
	if err != nil {
		return nil, err
	}
	today, localDay := dateOnly(now), localDate(now)
	state := ledger.fold
	rebuildReason := ""
	switch {
	case state == nil:
		rebuildReason = "usage totals were not loaded"
	case !state.today.Equal(today):
		rebuildReason = "the UTC date changed"
	case !state.localDay.Equal(localDay):
		rebuildReason = "the local date changed"
	case !state.standardStart.Equal(standardStart):
		rebuildReason = "the dates used to value OpenAI usage changed"
	case state.marks.MutationGeneration != marks.MutationGeneration:
		rebuildReason = "recorded usage was deleted or a session file was rewritten"
	case !maps.Equal(state.providerByKey, providers) || !maps.Equal(state.keysByProvider, keys):
		rebuildReason = "an account's identity changed"
	case consultedOwnerChanged(state, owners):
		rebuildReason = "the account of an allowance window that attributed usage changed"
	case !state.deferredUntil.IsZero() && !now.Before(state.deferredUntil):
		rebuildReason = "a previously future-dated usage record became current"
	}
	refold := rebuildReason != ""
	if !refold {
		// New windows, or windows no counted usage went through, change nothing.
		state.allowanceOwners = owners
	}
	var rebuildStarted time.Time
	if refold {
		rebuildStarted = time.Now()
		state = newUsageFold(today, localDay, standardStart, providers, keys, owners)
	}
	err = ledger.foldRows(ctx, transaction, state, marks, now, refold)
	if lateReason, ok := err.(usageRefoldReason); ok {
		rebuildReason = string(lateReason)
		rebuildStarted = time.Now()
		state = newUsageFold(today, localDay, standardStart, providers, keys, owners)
		err = ledger.foldRows(ctx, transaction, state, marks, now, true)
	}
	if err != nil {
		ledger.fold = nil
		return nil, err
	}
	if err = transaction.Commit(); err != nil {
		ledger.fold = nil
		return nil, err
	}
	if rebuildReason != "" {
		// A full fold briefly holds every proxy; release their now-empty buckets.
		pending := make(map[uint64]pendingUsageContribution, len(state.pending))
		for response, contribution := range state.pending {
			pending[response] = contribution
		}
		state.pending = pending
	}
	state.marks = marks
	ledger.fold = state
	if rebuildReason != "" {
		log.Printf("usage totals rebuilt from history in %s: %s", time.Since(rebuildStarted).Round(time.Millisecond), rebuildReason)
	}
	return state, nil
}

func readUsageIdentities(ctx context.Context, query usageQuery, pool []accounts.PoolAccount) (map[store.AccountKey]store.ProviderAccountID, map[store.ProviderAccountID]store.AccountKey, map[allowanceIdentityKey]store.ProviderAccountID, error) {
	providers := make(map[store.AccountKey]store.ProviderAccountID)
	keys := make(map[store.ProviderAccountID]store.AccountKey)
	rows, err := query.QueryContext(ctx, `SELECT account_key,provider_account_id FROM account_identities WHERE account_key IS NOT NULL ORDER BY account_name`)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var key store.AccountKey
		var provider store.ProviderAccountID
		if err = rows.Scan(&key, &provider); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		providers[key] = provider
		keys[provider] = key
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, nil, err
	}
	for _, account := range pool {
		provider := store.ProviderAccountID(account.AccountID)
		providers[account.Key] = provider
		keys[provider] = account.Key
	}
	owners := make(map[allowanceIdentityKey]store.ProviderAccountID)
	rows, err = query.QueryContext(ctx, `SELECT duration_seconds,resets_at_unix,CASE WHEN COUNT(DISTINCT provider_account_id)=1 THEN MIN(provider_account_id) ELSE '' END FROM account_allowance_window_identities GROUP BY duration_seconds,resets_at_unix`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key allowanceIdentityKey
		var owner store.ProviderAccountID
		if err = rows.Scan(&key.seconds, &key.reset, &owner); err != nil {
			return nil, nil, nil, err
		}
		owners[key] = owner
	}
	return providers, keys, owners, rows.Err()
}

func (ledger *UsageLedger) foldRows(ctx context.Context, query usageQuery, state *usageFold, marks usageRowWatermarks, now time.Time, refold bool) error {
	start := state.today.AddDate(0, 0, 1-reportingPeriodDays)
	if err := streamNativeSources(ctx, query, state.marks.NativeSources, marks.NativeSources, func(_ int64, metadata taskMetadata) error { state.metadata[metadata.TaskID] = &metadata; return nil }); err != nil {
		return err
	}
	if err := streamNativeChildren(ctx, query, state.marks.NativeChildren, marks.NativeChildren, func(_ int64, parent, child string) error {
		if state.childParents[child] == nil {
			state.childParents[child] = make(map[string]bool)
		}
		state.childParents[child][parent] = true
		return nil
	}); err != nil {
		return err
	}
	proxyStart := start
	if !state.standardStart.IsZero() && state.standardStart.Before(proxyStart) {
		proxyStart = state.standardStart
	}
	if err := store.StreamCompletedResponses(ctx, query, state.marks.CompletedResponses, marks.CompletedResponses, proxyStart, lastUsageTime, func(_ int64, response store.CompletedResponseRecord) error {
		if !state.standardStart.IsZero() && !response.CompletedAt.Before(state.standardStart) && response.HarnessMode != string(harnessFast) {
			cost := apiEquivalentPrice(response.ModelSlug, harnessStandard, tokenUsage{InputTokens: response.InputTokens, CachedInputTokens: response.CachedInputTokens, OutputTokens: response.OutputTokens})
			if cost != nil {
				account := string(response.AccountKey)
				if state.standardCost[account] == nil {
					state.standardCost[account] = make(map[modelDay]nanoUSD)
				}
				state.standardCost[account][modelDay{response.CompletedAt.UTC().Format(time.DateOnly), response.ModelSlug, false}] += *cost
			}
		}
		if response.CompletedAt.Before(start) {
			return nil
		}
		hash := maphash.String(state.hashSeed, string(response.CompletedResponseID))
		if state.responses[hash] != 0 {
			return usageRefoldReason("a proxy response arrived after its native usage")
		}
		task := state.canonicalTaskID(string(response.TaskID))
		if legacy := state.latestLegacy[task]; !legacy.IsZero() && !legacy.Before(response.CompletedAt) {
			return usageRefoldReason("a proxy response arrived inside legacy session history")
		}
		if first := state.firstProxy[task]; first.IsZero() || response.CompletedAt.Before(first) {
			state.firstProxy[task] = response.CompletedAt
		}
		completion := proxyCompletion(response)
		var allowance *sessionAllowance
		needsAllowance := state.providerByKey[completion.AccountKey] == ""
		if needsAllowance {
			var err error
			allowance, err = latestResponseAllowance(ctx, query, string(response.CompletedResponseID), start, now)
			if err != nil {
				return err
			}
		}
		attributeCompletion(&completion, allowance, state.providerByKey, state.allowanceOwners, state.consultedAllowanceWindows)
		state.canonicalAccount(&completion)
		completion.TaskID = state.canonicalTaskID(completion.TaskID)
		key, amount := completionCohort(completion)
		if strings.HasPrefix(task, "claude:") {
			addCohort(state.settled, key, amount)
		} else {
			state.pending[hash] = pendingUsageContribution{key, amount}
		}
		state.responses[hash] = countedProxy
		if needsAllowance {
			state.responses[hash] |= countedNeedsAllowance
		}
		if completion.Cost == nil {
			ledger.logOnChange("price/"+completion.Model, fmt.Sprintf("model %s has no API-equivalent price; add it to the rate card", completion.Model))
		}
		if !response.CompletedAt.Before(now) {
			state.deferUntil(response.CompletedAt)
			return nil
		}
		if !response.CompletedAt.Before(state.localDay) {
			// Proxy activity prices only the proxy's token fields, as before.
			response.CacheWriteInputTokens = nil
			activity := proxyCompletion(response)
			key, amount := completionCohort(activity)
			key.Date = state.localDay
			addCohort(state.proxyActivity, key, amount)
		}
		return nil
	}); err != nil {
		return err
	}
	if refold {
		// Allowance-only rows are read by response when attributing usage. Their
		// future timestamps still determine when previously read data becomes eligible.
		var nextObservation sql.NullInt64
		if err := query.QueryRowContext(ctx, `SELECT MIN(observed) FROM native_usage_observations WHERE observed>=?`, now.UnixNano()).Scan(&nextObservation); err != nil {
			return err
		}
		if nextObservation.Valid {
			state.deferUntil(time.Unix(0, nextObservation.Int64))
		}
	}
	return streamNativeUsageWithResponses(ctx, query, state.marks.NativeObservations, marks.NativeObservations, start, lastUsageTime, refold, func(_ int64, observation sessionUsageObservation, proxy *store.CompletedResponseRecord) error {
		if !observation.At.Before(now) {
			state.deferUntil(observation.At)
			return nil
		}
		hash := maphash.String(state.hashSeed, observation.ResponseID)
		counted := state.responses[hash]
		if observation.Usage == nil {
			if !refold && observation.Allowance != nil && counted&countedNeedsAllowance != 0 {
				return usageRefoldReason("an allowance arrived after usage that needed it for attribution")
			}
			return nil
		}
		if observation.ResponseID == "" {
			if !refold {
				return usageRefoldReason("session usage arrived without a response ID")
			}
			if first := state.firstProxy[observation.TaskID]; !first.IsZero() && !observation.At.Before(first) {
				return fmt.Errorf("native usage without response identity overlaps proxy history: %s at %d", observation.SourcePath, observation.SourceOffset)
			}
			if observation.At.After(state.latestLegacy[observation.TaskID]) {
				state.latestLegacy[observation.TaskID] = observation.At
			}
		} else if counted&countedNative != 0 {
			if !refold {
				return usageRefoldReason("a session repeated a response already counted")
			}
			return nil // The first native usage row in time/source order is authoritative.
		}
		completion, err := nativeCompletion(&observation, proxy)
		if err != nil {
			return err
		}
		allowance := observation.Allowance
		needsAllowance := state.providerByKey[completion.AccountKey] == ""
		if needsAllowance && observation.ResponseID != "" {
			latest, err := latestResponseAllowance(ctx, query, observation.ResponseID, start, now)
			if err != nil {
				return err
			}
			if latest != nil {
				allowance = latest
			}
		}
		attributeCompletion(&completion, allowance, state.providerByKey, state.allowanceOwners, state.consultedAllowanceWindows)
		state.canonicalAccount(&completion)
		completion.TaskID = state.canonicalTaskID(completion.TaskID)
		key, amount := completionCohort(completion)
		if proxy != nil {
			delete(state.pending, hash)
		}
		addCohort(state.settled, key, amount)
		if completion.Cost == nil {
			ledger.logOnChange("price/"+completion.Model, fmt.Sprintf("model %s has no API-equivalent price; add it to the rate card", completion.Model))
		}
		if observation.ResponseID != "" {
			state.responses[hash] = counted | countedNative
			if needsAllowance {
				state.responses[hash] |= countedNeedsAllowance
			}
		}
		if observation.TurnID != "" {
			previous, exists := state.turnOwners[observation.TurnID]
			at := observation.At.UnixNano()
			if !exists || at > previous.At || (at == previous.At && (observation.TaskID > previous.TaskID || (observation.TaskID == previous.TaskID && observation.SourceOffset > previous.Offset))) {
				state.turnOwners[observation.TurnID] = usageTurnOwner{completion.TaskID, at, observation.SourceOffset}
			}
		}
		return nil
	})
}

func (state *usageFold) canonicalAccount(completion *reconciledCompletion) {
	if key := state.keysByProvider[completion.ProviderAccountID]; key != "" {
		completion.AccountKey = key
	}
}

func (state *usageFold) deferUntil(at time.Time) {
	if state.deferredUntil.IsZero() || at.Before(state.deferredUntil) {
		state.deferredUntil = at
	}
}

func completionCohort(completion reconciledCompletion) (usageCohortKey, usageCohortAmount) {
	key := usageCohortKey{TaskID: completion.TaskID, Account: completion.AccountKey, Date: dateOnly(completion.At)}
	if completion.NativeSource != nil {
		key.RootTurnID = completion.NativeSource.RootTurnID
	}
	latest := completion.LastCompletedAt
	if latest.IsZero() {
		latest = completion.At
	}
	amount := usageCohortAmount{Tokens: completion.Tokens, Count: 1, LastCompletedAt: latest}
	if completion.Cost == nil {
		amount.UnknownPrices = 1
	} else {
		amount.Cost = *completion.Cost
	}
	if completion.AccountKey == "" {
		amount.Unattributed = 1
	}
	return key, amount
}

func addCohort(cohorts map[usageCohortKey]usageCohortAmount, key usageCohortKey, amount usageCohortAmount) {
	total := cohorts[key]
	total.add(amount)
	cohorts[key] = total
}

func latestResponseAllowance(ctx context.Context, query usageQuery, responseID string, start, end time.Time) (*sessionAllowance, error) {
	var allowance sessionAllowance
	var primaryUsed, secondaryUsed sql.NullFloat64
	var primaryMinutes, primaryResets, secondaryMinutes, secondaryResets sql.NullInt64
	err := query.QueryRowContext(ctx, `SELECT limit_id,plan_type,primary_used,primary_minutes,primary_resets,secondary_used,secondary_minutes,secondary_resets FROM native_usage_observations o JOIN native_usage_sources s ON s.task_id=o.task_id WHERE o.response_id=? AND o.response_id!='' AND o.observed>=? AND o.observed<? AND o.limit_id IS NOT NULL ORDER BY o.observed DESC,o.task_id DESC,o.source_offset DESC LIMIT 1`, responseID, start.UnixNano(), end.UnixNano()).Scan(&allowance.LimitID, &allowance.PlanType, &primaryUsed, &primaryMinutes, &primaryResets, &secondaryUsed, &secondaryMinutes, &secondaryResets)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if primaryUsed.Valid {
		allowance.Primary = &sessionAllowanceWindow{primaryUsed.Float64, primaryMinutes.Int64, time.Unix(0, primaryResets.Int64)}
	}
	if secondaryUsed.Valid {
		allowance.Secondary = &sessionAllowanceWindow{secondaryUsed.Float64, secondaryMinutes.Int64, time.Unix(0, secondaryResets.Int64)}
	}
	return &allowance, nil
}

// Iterate the settled and pending sums directly: reading a report needs no
// second copy of the cohort table.
func (state *usageFold) reportingCohorts() iter.Seq2[usageCohortKey, usageCohortAmount] {
	return func(yield func(usageCohortKey, usageCohortAmount) bool) {
		for key, amount := range state.settled {
			if !yield(key, amount) {
				return
			}
		}
		for _, pending := range state.pending {
			if !yield(pending.Cohort, pending.Amount) {
				return
			}
		}
	}
}

func (ledger *UsageLedger) foldMetadata(state *usageFold) (taskMetadataLookup, error) {
	for task, item := range state.metadata {
		if item.TaskID != "" && !strings.HasPrefix(item.SourceKind, "claude:") && item.ParentTaskID == "" && len(state.childParents[task]) > 1 {
			return nil, fmt.Errorf("conflicting native parent records for task %s", task)
		}
	}
	err := ledger.visitClaudeTaskMetadata(maps.Keys(state.firstProxy), func(item taskMetadata) {
		if previous := state.metadata[item.TaskID]; previous == nil || *previous != item {
			state.metadata[item.TaskID] = &item
		}
	})
	if err != nil {
		return nil, err
	}
	for _, item := range state.metadata {
		if item.SourceKind == piSessionSourceKind {
			item.Title, err = ledger.piSessionTitle(item.SourcePath)
			if err != nil {
				return nil, err
			}
		}
	}
	return func(task string) taskMetadata {
		metadata := state.metadata[task]
		if metadata == nil {
			return taskMetadata{}
		}
		item := *metadata
		if item.TaskID != "" && !strings.HasPrefix(item.SourceKind, "claude:") && item.ParentTaskID == "" {
			for parent := range state.childParents[task] {
				item.ParentTaskID = parent
			}
		}
		return item
	}, nil
}

func (state *usageFold) canonicalTaskID(task string) string {
	if metadata := state.metadata[task]; metadata != nil {
		return metadata.TaskID
	}
	return task
}

func (state *usageFold) turnOwnerTask(turn string) string {
	return state.turnOwners[turn].TaskID
}

// consultedOwnerChanged reports whether a window some counted usage was
// attributed through now has a different owner, or none.
func consultedOwnerChanged(state *usageFold, owners map[allowanceIdentityKey]store.ProviderAccountID) bool {
	for key := range state.consultedAllowanceWindows {
		if state.allowanceOwners[key] != owners[key] {
			return true
		}
	}
	return false
}
