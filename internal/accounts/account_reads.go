package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

var errWhamUsageUnauthorized = errors.New("wham/usage rejected account credentials")

// A provider that refuses another allowance read for now has still answered:
// this says nothing about the account or the build reading it.
var errUsageReadThrottled = errors.New("usage read throttled")

// observeAccount reads current credentials, allowance and, when readModelList
// is set, model availability, starting from the fact as it was when the read
// was scheduled. Failures are carried on the fact, and the monitor logs a
// change of state once: the provider, not a monitoring gap, decides limits.
func (monitor *Monitor) observeAccount(parent context.Context, account PoolAccount, savedLogin *savedLogin, fact AccountRoutingFact, readModelList bool) AccountRoutingFact {
	if fact.Credentials.AccountID != account.AccountID {
		fact = AccountRoutingFact{Credentials: Credentials{AccountID: account.AccountID}}
	}
	fact.AccountName = store.AccountName(account.Name)
	fact.AccountStateCause = nil
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(parent, accountReadTimeout)
	defer cancel()
	credentials, err := savedLogin.Credentials(ctx)
	if err != nil {
		if requiresSignIn(err) || fact.AccountState == AccountSignInRequired {
			fact.AccountState = AccountSignInRequired
		} else {
			fact.AccountState = AccountCredentialsUnavailable
		}
		fact.AccountStateCause = err
		return fact
	}
	fact.Credentials = credentials

	sample, err := monitor.readAllowanceSample(ctx, account, credentials)
	if errors.Is(err, errWhamUsageUnauthorized) {
		credentials, err = savedLogin.RefreshAfterUnauthorized(ctx, credentials)
		if err == nil {
			fact.Credentials = credentials
			sample, err = monitor.readAllowanceSample(ctx, account, credentials)
		}
	}
	if err != nil {
		if requiresSignIn(err) || fact.AccountState == AccountSignInRequired {
			fact.AccountState = AccountSignInRequired
		} else if fact.AllowanceSample.ObservedAt.IsZero() {
			fact.AccountState = AccountUsageCheckFailed
		} else if fact.AllowanceSample.LimitReached || !fact.AllowanceSample.Allowed {
			fact.AccountState = AccountLimited
		} else {
			fact.AccountState = AccountReady
		}
		fact.AccountStateCause = err
	} else if sample.LimitReached || !sample.Allowed {
		fact.AccountState = AccountLimited
	} else {
		fact.AccountState = AccountReady
	}
	if err == nil {
		fact.AllowanceSample = sample
		fact.RefreshedAt = startedAt
	}

	if !readModelList {
		return fact
	}
	modelContext, cancelModels := context.WithTimeout(parent, accountReadTimeout)
	defer cancelModels()
	models, modelErr := monitor.readSupportedModelSlugs(modelContext, fact.Credentials)
	fact.ModelListCause = modelErr
	if modelErr == nil {
		fact.SupportedModelSlugs = models
	}
	return fact
}

// readAllowanceSample reads wham/usage and records each used allowance window
// with an explicit reset, so usage can tell which account owned it.
func (monitor *Monitor) readAllowanceSample(ctx context.Context, account PoolAccount, credentials Credentials) (AllowanceSample, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, monitor.usageURL, nil)
	if err != nil {
		return AllowanceSample{}, err
	}
	request.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	request.Header.Set("ChatGPT-Account-ID", credentials.AccountID)
	response, err := monitor.httpClient.Do(request)
	if err != nil {
		return AllowanceSample{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		statusError := fmt.Errorf("wham/usage returned HTTP %d for %s", response.StatusCode, account.DisplayName)
		if response.StatusCode == http.StatusUnauthorized {
			return AllowanceSample{}, fmt.Errorf("%w: %v", errWhamUsageUnauthorized, statusError)
		}
		if response.StatusCode == http.StatusTooManyRequests {
			return AllowanceSample{}, fmt.Errorf("%w: %v", errUsageReadThrottled, statusError)
		}
		return AllowanceSample{}, statusError
	}
	var usage usageResponse
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		return AllowanceSample{}, err
	}
	sample, err := usage.Sample(credentials.AccountID, time.Now())
	if err != nil {
		return AllowanceSample{}, err
	}
	monitor.recordWeeklyAllowance(account.Name, sample)
	for _, window := range []*usageWindow{usage.RateLimit.Primary, usage.RateLimit.Secondary} {
		if window == nil || *window.UsedPercent <= 0 || window.ResetAt == nil || *window.ResetAt <= 0 {
			continue
		}
		if err := monitor.storeDatabase.RecordAccountAllowanceWindowIdentity(store.AccountAllowanceWindowIdentity{
			ProviderAccountID: store.ProviderAccountID(sample.AccountID),
			DurationSeconds:   *window.DurationSeconds,
			ResetsAt:          time.Unix(*window.ResetAt, 0).UTC(),
			ObservedAt:        sample.ObservedAt,
			UsedPercent:       *window.UsedPercent,
		}); err != nil {
			log.Printf("%s: could not record its allowance window: %v", account.DisplayName, err)
		}
	}
	return sample, nil
}

func (monitor *Monitor) recordWeeklyAllowance(accountName string, sample AllowanceSample) {
	for _, window := range sample.Windows {
		if window.DurationSeconds != 7*86400 || window.ModelID != "" || window.ModelName != "" || window.ResetsAt.IsZero() {
			continue
		}
		if err := monitor.storeDatabase.RecordWeeklyAllowanceReading(store.WeeklyAllowanceReading{
			AccountName: store.AccountName(accountName), ResetsAt: window.ResetsAt,
			UsedPercent: window.UsedPercent, ObservedAt: sample.ObservedAt,
		}); err != nil {
			log.Printf("%s: could not record weekly allowance: %v", accountName, err)
		}
	}
}

// readSupportedModelSlugs reads the model catalogue Codex itself would see, so
// work for a model is never routed to an account that does not offer it.
func (monitor *Monitor) readSupportedModelSlugs(ctx context.Context, credentials Credentials) ([]string, error) {
	clientVersion, err := codexClientVersion(ctx, monitor.codexExecutable)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, monitor.modelsURL, nil)
	if err != nil {
		return nil, err
	}
	query := request.URL.Query()
	query.Set("client_version", clientVersion)
	request.URL.RawQuery = query.Encode()
	request.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	request.Header.Set("ChatGPT-Account-ID", credentials.AccountID)
	response, err := monitor.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models returned HTTP %d", response.StatusCode)
	}
	var catalogue struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&catalogue); err != nil {
		return nil, err
	}
	if catalogue.Models == nil {
		return nil, errors.New("models response omitted models")
	}
	slugs := make([]string, 0, len(catalogue.Models))
	for _, model := range catalogue.Models {
		if model.Slug == "" {
			return nil, errors.New("models response contains empty slug")
		}
		slugs = append(slugs, model.Slug)
	}
	return slugs, nil
}
