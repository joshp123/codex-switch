package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ResetDecision reports the result of an explicit reset request.
type ResetDecision struct{ Message string }
type ResetCredit struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	ResetType string    `json:"reset_type"`
	ExpiresAt time.Time `json:"expires_at"`
}

// pendingReset is a reset whose reply was lost, kept in this Mac's data
// directory until a retry learns its outcome, so the folder names its login.
// Every build reads and writes it under these JSON names, so a build after an
// update or a rollback finishes a reset another build began.
type pendingReset struct {
	Folder    string `json:"AccountName"`
	AccountID string
	CreditID  string
}

var resetNamespace = uuid.MustParse("6f1c2a9e-2d4b-4c1e-9a7f-3b8d5e0c1a42")

// ResetCodexPool reads current provider state and spends one credit only when
// every account is limited and none is due to become available within 30 minutes.
func ResetCodexPool(ctx context.Context, directory string, origin *url.URL, client *http.Client) (decision ResetDecision, err error) {
	err = withFileLock(filepath.Join(directory, "reset.lock"), func() error {
		pendingPath := filepath.Join(directory, "pending-reset.json")
		if data, readErr := os.ReadFile(pendingPath); readErr == nil {
			var pending pendingReset
			if err := json.Unmarshal(data, &pending); err != nil {
				return fmt.Errorf("read pending reset: %w", err)
			}
			unspent, err := pendingCreditUnspent(ctx, directory, origin, client, pending)
			if err != nil {
				return fmt.Errorf("reset outcome unknown; run the command again to retry: %w", err)
			}
			if !unspent {
				return completeReset(ctx, directory, origin, client, pendingPath, pending, &decision)
			}
			// The lost request never spent the credit, so the pool may no longer
			// need it: decide again from current allowance.
			if err := os.Remove(pendingPath); err != nil {
				return err
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		pool, err := ReadPool(directory)
		if err != nil {
			return err
		}
		now := time.Now()
		count := 0
		type candidate struct {
			account PoolAccount
			credit  ResetCredit
		}
		var candidates []candidate
		var nextAvailable time.Time
		var nextAccount string
		for _, account := range pool {
			if account.Provider != ProviderCodex {
				continue
			}
			count++
			login := newSavedLogin(filepath.Join(directory, account.Folder), client)
			credentials, err := login.Credentials(ctx)
			if err != nil {
				return fmt.Errorf("%s: cannot check credentials: %w", account.DisplayName, err)
			}
			var usage usageResponse
			if err := resetRequest(ctx, client, origin, credentials, http.MethodGet, "/backend-api/wham/usage", nil, &usage); err != nil {
				return fmt.Errorf("%s: cannot check allowance: %w", account.DisplayName, err)
			}
			sample, err := usage.Sample(credentials.AccountID, now)
			if err != nil {
				return fmt.Errorf("%s: %w", account.DisplayName, err)
			}
			if sample.Allowed && !sample.LimitReached {
				decision.Message = fmt.Sprintf("No reset: %s can continue working.", account.DisplayName)
				return nil
			}
			// All windows at their limit must reset before this account can work.
			var availableAt time.Time
			for _, window := range sample.Windows {
				if window.UsedPercent >= 100 && window.ResetsAt.After(availableAt) {
					availableAt = window.ResetsAt
				}
			}
			if availableAt.IsZero() || !availableAt.After(now) {
				return fmt.Errorf("%s: limited but its reset time is unknown", account.DisplayName)
			}
			if nextAvailable.IsZero() || availableAt.Before(nextAvailable) {
				nextAvailable, nextAccount = availableAt, account.DisplayName
			}
			if !availableAt.After(time.Now().Add(30 * time.Minute)) {
				decision.Message = fmt.Sprintf("No reset: %s becomes available at %s, within 30 minutes.", account.DisplayName, availableAt.Local().Format(time.RFC1123))
				return nil
			}
			var list struct {
				Credits []ResetCredit `json:"credits"`
			}
			if err := resetRequest(ctx, client, origin, credentials, http.MethodGet, "/backend-api/wham/rate-limit-reset-credits", nil, &list); err != nil {
				return fmt.Errorf("%s: cannot check reset credits: %w", account.DisplayName, err)
			}
			for _, credit := range list.Credits {
				if credit.ID != "" && credit.Status == "available" && credit.ResetType == "codex_rate_limits" && credit.ExpiresAt.After(now) {
					candidates = append(candidates, candidate{account, credit})
				}
			}
		}
		if count == 0 {
			decision.Message = "No reset: there are no Codex accounts."
			return nil
		}
		if !nextAvailable.After(time.Now().Add(30 * time.Minute)) {
			decision.Message = fmt.Sprintf("No reset: %s becomes available at %s, within 30 minutes.", nextAccount, nextAvailable.Local().Format(time.RFC1123))
			return nil
		}
		if len(candidates) == 0 {
			decision.Message = "No reset: no available Codex reset credits."
			return nil
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].credit.ExpiresAt.Before(candidates[j].credit.ExpiresAt) })
		chosen := candidates[0]
		pending := pendingReset{chosen.account.Folder, chosen.account.AccountID, chosen.credit.ID}
		data, err := json.Marshal(pending)
		if err != nil {
			return err
		}
		if err := writeAtomicData(pendingPath, data); err != nil {
			return err
		}
		return completeReset(ctx, directory, origin, client, pendingPath, pending, &decision)
	})
	return
}

func pendingResetAccount(ctx context.Context, directory string, client *http.Client, pending pendingReset) (PoolAccount, Credentials, error) {
	pool, err := ReadPool(directory)
	if err != nil {
		return PoolAccount{}, Credentials{}, err
	}
	for _, account := range pool {
		if account.Folder == pending.Folder && account.AccountID == pending.AccountID && account.Provider == ProviderCodex {
			credentials, err := newSavedLogin(filepath.Join(directory, account.Folder), client).Credentials(ctx)
			return account, credentials, err
		}
	}
	return PoolAccount{}, Credentials{}, fmt.Errorf("pending reset account is no longer in the pool")
}

// pendingCreditUnspent reports whether the credit of a reset whose response
// was lost is still listed as available, meaning the request never spent it.
func pendingCreditUnspent(ctx context.Context, directory string, origin *url.URL, client *http.Client, pending pendingReset) (bool, error) {
	_, credentials, err := pendingResetAccount(ctx, directory, client, pending)
	if err != nil {
		return false, err
	}
	var list struct {
		Credits []ResetCredit `json:"credits"`
	}
	if err := resetRequest(ctx, client, origin, credentials, http.MethodGet, "/backend-api/wham/rate-limit-reset-credits", nil, &list); err != nil {
		return false, err
	}
	for _, credit := range list.Credits {
		if credit.ID == pending.CreditID {
			return credit.Status == "available", nil
		}
	}
	return false, nil
}

func completeReset(ctx context.Context, directory string, origin *url.URL, client *http.Client, path string, pending pendingReset, decision *ResetDecision) error {
	account, credentials, err := pendingResetAccount(ctx, directory, client, pending)
	if err != nil {
		return err
	}
	requestID := uuid.NewSHA1(resetNamespace, []byte(pending.AccountID+":"+pending.CreditID)).String()
	body, _ := json.Marshal(map[string]string{"credit_id": pending.CreditID, "redeem_request_id": requestID})
	var result struct {
		Code string `json:"code"`
	}
	if err := resetRequest(ctx, client, origin, credentials, http.MethodPost, "/backend-api/wham/rate-limit-reset-credits/consume", body, &result); err != nil {
		return fmt.Errorf("reset outcome unknown; run the command again to retry the same credit: %w", err)
	}
	switch result.Code {
	case "reset", "already_redeemed", "nothing_to_reset", "no_credit":
		if err := os.Remove(path); err != nil {
			return err
		}
		outcomes := map[string]string{"reset": "reset", "already_redeemed": "credit was already redeemed", "nothing_to_reset": "nothing to reset", "no_credit": "credit is no longer available"}
		decision.Message = fmt.Sprintf("Codex pool exhausted; no account was due to reset within 30 minutes. %s: %s.", account.DisplayName, outcomes[result.Code])
		return nil
	default:
		return fmt.Errorf("reset outcome unknown; run the command again to retry the same credit (provider result %q)", result.Code)
	}
}

func resetRequest(ctx context.Context, client *http.Client, origin *url.URL, credentials Credentials, method, path string, body []byte, result any) error {
	endpoint := origin.ResolveReference(&url.URL{Path: path})
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	request.Header.Set("ChatGPT-Account-ID", credentials.AccountID)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("OpenAI-Beta", "codex-1")
	request.Header.Set("Originator", "Codex Desktop")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("provider returned HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result)
}
