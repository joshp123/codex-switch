package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

const claudeUsageReadInterval = 5 * time.Minute

// SetClaudeUpstreamOrigin changes the Claude origin before Run starts.
func (monitor *Monitor) SetClaudeUpstreamOrigin(origin *url.URL) { monitor.claudeOrigin = origin }

type claudeAllowanceCache struct {
	Sample       AllowanceSample
	Restrictions map[string]ClaudeRestriction
	ReadAfter    time.Time
}

func (monitor *Monitor) observeClaudeAccount(parent context.Context, account PoolAccount, login *savedLogin, fact AccountRoutingFact) AccountRoutingFact {
	if fact.Credentials.AccountID != account.AccountID {
		fact = AccountRoutingFact{Credentials: Credentials{AccountID: account.AccountID}}
	}
	fact.AccountName, fact.Provider = store.AccountName(account.Name), ProviderClaude
	fact.AllowanceSample.AccountID, fact.AllowanceSample.Plan = account.AccountID, account.Plan
	ctx, cancel := context.WithTimeout(parent, accountReadTimeout)
	defer cancel()
	credentials, err := login.Credentials(ctx)
	if err != nil {
		fact.AccountState, fact.AccountStateCause = AccountCredentialsUnavailable, err
		if requiresSignIn(err) {
			fact.AccountState = AccountSignInRequired
		}
		return fact
	}
	if fact.AccountState == AccountSignInRequired || fact.AccountState == AccountCredentialsUnavailable {
		fact.AccountStateCause = nil
	}
	fact.Credentials, fact.AccountState = credentials, AccountReady
	startedAt := time.Now()
	cache, due, err := monitor.reserveClaudeUsageRead(account.Name, startedAt)
	if err != nil {
		fact.AccountStateCause = err
		updateClaudeSharedState(&fact, time.Now())
		return fact
	}
	fact.AllowanceSample = mergeClaudeAllowance(fact.AllowanceSample, cache.Sample)
	fact.ClaudeRestrictions = mergeClaudeRestrictions(fact.ClaudeRestrictions, cache.Restrictions)
	if due {
		var sample AllowanceSample
		var retryAt time.Time
		sample, retryAt, err = monitor.readClaudeAllowance(ctx, account, credentials)
		if requiresSignIn(err) {
			credentials, err = login.RefreshAfterUnauthorized(ctx, credentials)
			if err == nil {
				fact.Credentials = credentials
				sample, retryAt, err = monitor.readClaudeAllowance(ctx, account, credentials)
			}
		}
		fact.AccountStateCause = err
		if err == nil {
			fact.AllowanceSample = mergeClaudeAllowance(fact.AllowanceSample, sample)
			fact.RefreshedAt = startedAt
			clearClaudeRestrictionsFromSample(&fact, sample)
		} else if requiresSignIn(err) {
			fact.AccountState = AccountSignInRequired
		}
		if retryAt.After(cache.ReadAfter) {
			cache.ReadAfter = retryAt
		}
		cache.Sample, cache.Restrictions = fact.AllowanceSample, fact.ClaudeRestrictions
		if saveErr := monitor.saveClaudeAllowanceCache(account.Name, cache); saveErr != nil {
			log.Printf("%s: could not save Claude allowance: %v", account.DisplayName, saveErr)
		}
	}
	updateClaudeSharedState(&fact, time.Now())
	return fact
}

func clearClaudeRestrictionsFromSample(fact *AccountRoutingFact, sample AllowanceSample) {
	fact.ClaudeRestrictions = mergeClaudeRestrictions(nil, fact.ClaudeRestrictions)
	for scope, restriction := range fact.ClaudeRestrictions {
		for _, window := range sample.Windows {
			if claudeWindowRestrictionScope(window) == scope && !window.LimitReached && window.UsedPercent < 100 && window.ObservedAt.After(restriction.ObservedAt) {
				fact.ClaudeRestrictions[scope] = ClaudeRestriction{ObservedAt: window.ObservedAt}
			}
		}
	}
}

func (monitor *Monitor) readClaudeAllowance(ctx context.Context, account PoolAccount, credentials Credentials) (AllowanceSample, time.Time, error) {
	endpoint := monitor.claudeOrigin.ResolveReference(&url.URL{Path: "/api/oauth/usage"})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return AllowanceSample{}, time.Time{}, err
	}
	setClaudeOAuthHeaders(request, credentials.AccessToken)
	response, err := monitor.httpClient.Do(request)
	if err != nil {
		return AllowanceSample{}, time.Time{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, maximumRefreshResponseBytes))
		err := fmt.Errorf("Claude usage returned HTTP %d", response.StatusCode)
		if response.StatusCode == http.StatusUnauthorized {
			err = signInRequired(err)
		}
		if response.StatusCode == http.StatusTooManyRequests {
			err = fmt.Errorf("%w: %v", errUsageReadThrottled, err)
		}
		return AllowanceSample{}, claudeRetryAfter(response.Header, time.Now()), err
	}
	var usage claudeUsageResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maximumRefreshResponseBytes)).Decode(&usage); err != nil {
		return AllowanceSample{}, time.Time{}, err
	}
	sample, err := usage.sample(account, time.Now())
	if err == nil {
		monitor.recordWeeklyAllowance(account.Name, sample)
	}
	return sample, time.Time{}, err
}

func setClaudeOAuthHeaders(request *http.Request, token string) {
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("Accept", "application/json")
}

func (monitor *Monitor) reserveClaudeUsageRead(name string, now time.Time) (claudeAllowanceCache, bool, error) {
	var cache claudeAllowanceCache
	var due bool
	err := monitor.withClaudeAllowanceCache(name, func(existing claudeAllowanceCache) (claudeAllowanceCache, error) {
		cache = existing
		if !now.Before(existing.ReadAfter) {
			due = true
			cache.ReadAfter = now.Add(claudeUsageReadInterval)
		}
		return cache, nil
	})
	return cache, due, err
}

func (monitor *Monitor) saveClaudeAllowanceCache(name string, incoming claudeAllowanceCache) error {
	return monitor.withClaudeAllowanceCache(name, func(existing claudeAllowanceCache) (claudeAllowanceCache, error) {
		existing.Sample = mergeClaudeAllowance(existing.Sample, incoming.Sample)
		existing.Restrictions = mergeClaudeRestrictions(existing.Restrictions, incoming.Restrictions)
		if incoming.ReadAfter.After(existing.ReadAfter) {
			existing.ReadAfter = incoming.ReadAfter
		}
		return existing, nil
	})
}

func (monitor *Monitor) withClaudeAllowanceCache(name string, operation func(claudeAllowanceCache) (claudeAllowanceCache, error)) error {
	directory := filepath.Join(monitor.dataDirectory, name)
	return withFileLock(filepath.Join(directory, "claude-allowance.lock"), func() error {
		path := filepath.Join(directory, "claude-allowance.json")
		var cache claudeAllowanceCache
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if err := json.Unmarshal(data, &cache); err != nil {
				return err
			}
		}
		updated, err := operation(cache)
		if err != nil {
			return err
		}
		encoded, err := json.MarshalIndent(updated, "", "  ")
		if err != nil {
			return err
		}
		encoded = append(encoded, '\n')
		// Most calls only find the next read is not due yet; they leave the
		// file as it is.
		if bytes.Equal(encoded, data) {
			return nil
		}
		return writeAtomicData(path, encoded)
	})
}
