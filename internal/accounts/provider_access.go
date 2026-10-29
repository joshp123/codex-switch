package accounts

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// CheckProviderAccess reads each signed-in account's allowance from the provider.
// Deployment calls this before listening; cached allowances cannot admit a new generation.
func (monitor *Monitor) CheckProviderAccess(ctx context.Context) error {
	pool, err := ReadPool(monitor.dataDirectory)
	if err != nil {
		return err
	}
	results := make(chan error, len(pool))
	for _, account := range pool {
		go func() {
			readContext, cancel := context.WithTimeout(ctx, accountReadTimeout)
			defer cancel()
			login := newSavedLogin(filepath.Join(monitor.dataDirectory, account.Name), monitor.httpClient)
			login.provider = account.Provider
			var storedLoginError error
			if account.Provider == ProviderClaude {
				_, storedLoginError = login.readClaudeAuth()
			} else {
				_, storedLoginError = login.read()
			}
			if requiresSignIn(storedLoginError) {
				results <- nil
				return
			}
			credentials, err := login.Credentials(readContext)
			if err == nil {
				read := func() error {
					if account.Provider == ProviderClaude {
						_, _, err := monitor.readClaudeAllowance(readContext, account, credentials)
						return err
					}
					_, err := monitor.readAllowanceSample(readContext, account, credentials)
					return err
				}
				err = read()
				if errors.Is(err, errWhamUsageUnauthorized) || requiresSignIn(err) {
					credentials, err = login.RefreshAfterUnauthorized(readContext, credentials)
					if err == nil {
						err = read()
					}
				}
			}
			if requiresSignIn(err) {
				err = nil
			}
			if err != nil {
				err = fmt.Errorf("%s: %w", account.DisplayName, err)
			}
			results <- err
		}()
	}
	var failures []error
	for range pool {
		if err := <-results; err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
