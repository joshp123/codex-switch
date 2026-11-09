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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/joshp123/codex-switch/internal/logtime"
)

// A reset credit restores an account's weekly allowance, and OpenAI starts
// the new week at the account's first request afterwards. A credit is spent
// only when the pool is out or the credit is about to expire; see
// chooseResetCredit. Only the installation with automatic resets on spends one
// by itself; see automatic_resets.go.
const (
	// poolRefillWait is how long work waits for an account to refill by itself
	// before a credit is worth spending.
	poolRefillWait = 30 * time.Minute
	// expiringCreditWindow is a credit's last stretch before it expires, when
	// spending it beats losing it.
	expiringCreditWindow = time.Hour
)

// ResetDecision is what a reset request did, in words for the person or the
// proxy log. Spent is true when OpenAI confirmed that a credit this
// installation asked for was spent.
type ResetDecision struct {
	Message string
	Spent   bool
}

// ResetCredit is a credit as OpenAI lists it. OpenAI may list a credit
// without an expiry, which never expires: its ExpiresAt is zero.
type ResetCredit struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	ResetType string    `json:"reset_type"`
	ExpiresAt time.Time `json:"expires_at"`
}

// usable reports whether the credit can reset a Codex allowance at now.
func (credit ResetCredit) usable(now time.Time) bool {
	return credit.ID != "" && credit.Status == "available" && credit.ResetType == "codex_rate_limits" && (credit.neverExpires() || credit.ExpiresAt.After(now))
}

func (credit ResetCredit) neverExpires() bool {
	return credit.ExpiresAt.IsZero()
}

// expiresBefore orders credits by expiry, those that never expire last, then
// by ID.
func (credit ResetCredit) expiresBefore(other ResetCredit) bool {
	if !credit.ExpiresAt.Equal(other.ExpiresAt) {
		return other.neverExpires() || (!credit.neverExpires() && credit.ExpiresAt.Before(other.ExpiresAt))
	}
	return credit.ID < other.ID
}

func (credit ResetCredit) describeExpiry() string {
	if credit.neverExpires() {
		return "which never expires"
	}
	return "which expires at " + logtime.Format(credit.ExpiresAt)
}

// pendingReset is a request to spend a credit whose outcome is not known yet,
// kept in this Mac's data directory until a retry learns it, so the folder
// names its login. Every build reads and writes it under these JSON names, so
// a build after an update or a rollback finishes a reset another build began.
//
// RequestID is the attempt's redeem_request_id: a new attempt gets a new one,
// and a retry of the attempt sends it again, so OpenAI answers the retry with
// the attempt's outcome. A file without one was written by a build that
// derived the ID from the account and credit; a retry sends that ID. A build
// that ignores RequestID retries with the derived ID, which OpenAI answers as
// another request for the same credit: it cannot spend a second one.
type pendingReset struct {
	Folder    string `json:"AccountName"`
	AccountID string
	CreditID  string
	RequestID string `json:",omitempty"`
}

var resetNamespace = uuid.MustParse("6f1c2a9e-2d4b-4c1e-9a7f-3b8d5e0c1a42")

func (pending pendingReset) requestID() string {
	if pending.RequestID != "" {
		return pending.RequestID
	}
	return uuid.NewSHA1(resetNamespace, []byte(pending.AccountID+":"+pending.CreditID)).String()
}

// resetRules says which needs may spend a credit. The reset command spends
// only for an exhausted pool; the proxy also spends a credit about to expire,
// and spends for an exhausted pool only while work waits for an account.
type resetRules struct {
	whenPoolOut  bool
	beforeExpiry bool
}

// codexAccountReading is one Codex account as OpenAI reported it immediately
// before a spend decision.
type codexAccountReading struct {
	account   PoolAccount
	signedOut bool
	allowance AllowanceSample
	credits   []ResetCredit
}

type resetChoice struct {
	account PoolAccount
	credit  ResetCredit
	why     string
}

// chooseResetCredit is the one rule for spending a credit. Its choice depends
// only on what OpenAI reports, never on this Mac's folders or the order it
// reads accounts in.
//
// The pool is out when no Codex account can take work and none refills within
// 30 minutes. The credit then goes to the account with the most of its week
// left, the one that would refill last, using its credit that expires first.
//
// A credit in its last hour is spent when its account has used more of its
// week than the share of the week that has passed. OpenAI decides whether
// there is anything to reset: when there is not, it keeps the credit.
//
// With no credit to spend it returns why, or "" when no need arose.
func chooseResetCredit(readings []codexAccountReading, now time.Time, rules resetRules) (resetChoice, bool, string) {
	var reasons []string
	if rules.whenPoolOut {
		choice, found, reason := choosePoolOutCredit(readings, now)
		if found {
			return choice, true, ""
		}
		reasons = append(reasons, reason)
	}
	if rules.beforeExpiry {
		choice, found, reason := chooseExpiringCredit(readings, now)
		if found {
			return choice, true, ""
		}
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}
	return resetChoice{}, false, strings.Join(reasons, " ")
}

func choosePoolOutCredit(readings []codexAccountReading, now time.Time) (resetChoice, bool, string) {
	type candidate struct {
		reading   codexAccountReading
		credit    ResetCredit
		refillsAt time.Time
	}
	if len(readings) == 0 {
		return resetChoice{}, false, "No reset: there are no Codex accounts."
	}
	var candidates []candidate
	signedIn := 0
	for _, reading := range readings {
		if reading.signedOut {
			continue
		}
		signedIn++
		name := reading.account.DisplayName
		if IncludedAllowanceLeft(reading.allowance, now) {
			return resetChoice{}, false, fmt.Sprintf("No reset: %s can take work.", name)
		}
		refillsAt, known := refillTime(reading.allowance)
		if !known {
			return resetChoice{}, false, fmt.Sprintf("No reset: %s is limited, but OpenAI gives no time it refills.", name)
		}
		if !refillsAt.After(now.Add(poolRefillWait)) {
			return resetChoice{}, false, fmt.Sprintf("No reset: %s refills at %s, within 30 minutes.", name, logtime.Format(refillsAt))
		}
		if credit, found := firstExpiringCredit(reading.credits, now); found {
			candidates = append(candidates, candidate{reading, credit, refillsAt})
		}
	}
	switch {
	case signedIn == 0:
		return resetChoice{}, false, "No reset: no Codex account is signed in."
	case len(candidates) == 0:
		return resetChoice{}, false, "No reset: no Codex account has a reset credit."
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].refillsAt.Equal(candidates[j].refillsAt) {
			return candidates[i].refillsAt.After(candidates[j].refillsAt)
		}
		return candidates[i].reading.account.Key < candidates[j].reading.account.Key
	})
	chosen := candidates[0]
	why := fmt.Sprintf("no Codex account can take work and none refills within 30 minutes; %s has the most of its week left, refilling at %s, and this is its credit that expires first, %s",
		chosen.reading.account.DisplayName, logtime.Format(chosen.refillsAt), chosen.credit.describeExpiry())
	return resetChoice{chosen.reading.account, chosen.credit, why}, true, ""
}

func chooseExpiringCredit(readings []codexAccountReading, now time.Time) (resetChoice, bool, string) {
	type expiring struct {
		reading codexAccountReading
		credit  ResetCredit
		why     string
	}
	var spend, keep []expiring
	for _, reading := range readings {
		if reading.signedOut {
			continue
		}
		name := reading.account.DisplayName
		for _, credit := range reading.credits {
			if !credit.usable(now) || credit.neverExpires() || credit.ExpiresAt.After(now.Add(expiringCreditWindow)) {
				continue
			}
			week, known := currentWeek(reading.allowance, now)
			if !known {
				keep = append(keep, expiring{reading, credit, fmt.Sprintf("OpenAI reports no current week for %s", name)})
				continue
			}
			used, passed := week.UsedPercent, weekPassedPercent(week, now)
			switch {
			case used <= passed:
				keep = append(keep, expiring{reading, credit, fmt.Sprintf("%s has used %.0f%% of its week, no more than the %.0f%% of the week that has passed", name, used, passed)})
			default:
				spend = append(spend, expiring{reading, credit, fmt.Sprintf("it expires at %s, within the hour, and %s has used %.0f%% of its week while %.0f%% of the week has passed",
					logtime.Format(credit.ExpiresAt), name, used, passed)})
			}
		}
	}
	byExpiry := func(list []expiring) {
		sort.Slice(list, func(i, j int) bool { return list[i].credit.expiresBefore(list[j].credit) })
	}
	if len(spend) > 0 {
		byExpiry(spend)
		return resetChoice{spend[0].reading.account, spend[0].credit, spend[0].why}, true, ""
	}
	if len(keep) == 0 {
		return resetChoice{}, false, ""
	}
	byExpiry(keep)
	return resetChoice{}, false, fmt.Sprintf("Keeping reset credit %s on %s, which expires at %s: %s.",
		keep[0].credit.ID, keep[0].reading.account.DisplayName, logtime.Format(keep[0].credit.ExpiresAt), keep[0].why)
}

// refillTime is when a limited account can work again: when every window at
// its limit has reset.
func refillTime(sample AllowanceSample) (time.Time, bool) {
	var refillsAt time.Time
	for _, window := range sample.Windows {
		if window.UsedPercent >= 100 && window.ResetsAt.After(refillsAt) {
			refillsAt = window.ResetsAt
		}
	}
	return refillsAt, !refillsAt.IsZero()
}

func firstExpiringCredit(credits []ResetCredit, now time.Time) (ResetCredit, bool) {
	var first ResetCredit
	for _, credit := range credits {
		if !credit.usable(now) {
			continue
		}
		if first.ID == "" || credit.expiresBefore(first) {
			first = credit
		}
	}
	return first, first.ID != ""
}

// currentWeek is the longest current window that covers every model, which a
// credit restores. Its length comes from OpenAI, which changes allowances.
func currentWeek(sample AllowanceSample, now time.Time) (ReportedWindow, bool) {
	var week ReportedWindow
	for _, window := range sample.Windows {
		if window.ModelID != "" || window.ModelName != "" || window.DurationSeconds <= 0 || !window.ResetsAt.After(now) {
			continue
		}
		if window.DurationSeconds > week.DurationSeconds {
			week = window
		}
	}
	return week, week.DurationSeconds > 0
}

func weekPassedPercent(week ReportedWindow, now time.Time) float64 {
	length := time.Duration(week.DurationSeconds) * time.Second
	passed := 100 * (1 - week.ResetsAt.Sub(now).Seconds()/length.Seconds())
	return min(max(passed, 0), 100)
}

// ResetCodexPool spends one credit when the Codex pool is out; see
// chooseResetCredit.
func ResetCodexPool(ctx context.Context, directory string, origin *url.URL, client *http.Client) (ResetDecision, error) {
	return spendResetCredit(ctx, directory, origin, client, resetRules{whenPoolOut: true})
}

// spendResetCredit reads every Codex account from OpenAI and spends the credit
// chooseResetCredit picks under rules. It first finishes a request whose
// outcome is unknown, and spends nothing while OpenAI's usage reading does not
// show a reset this installation spent; see unconfirmedReset. reset.lock keeps
// the reset command and each proxy generation on this Mac from deciding at
// once.
func spendResetCredit(ctx context.Context, directory string, origin *url.URL, client *http.Client, rules resetRules) (decision ResetDecision, err error) {
	err = withFileLock(filepath.Join(directory, "reset.lock"), func() error {
		unconfirmed, err := readUnconfirmedReset(directory)
		if err != nil {
			return err
		}
		pendingPath := filepath.Join(directory, "pending-reset.json")
		if data, readErr := os.ReadFile(pendingPath); readErr == nil {
			var pending pendingReset
			if err := json.Unmarshal(data, &pending); err != nil {
				return fmt.Errorf("read pending reset: %w", err)
			}
			account, credentials, err := pendingResetAccount(ctx, directory, client, pending)
			if err != nil {
				return fmt.Errorf("reset outcome unknown; run the command again to retry: %w", err)
			}
			unspent, err := pendingCreditUnspent(ctx, origin, client, credentials, pending)
			if err != nil {
				return fmt.Errorf("reset outcome unknown; run the command again to retry: %w", err)
			}
			if !unspent {
				if unconfirmed == nil || unconfirmed.CreditID != pending.CreditID {
					// Another build began this reset. It wrote the file just before
					// it asked, so the reset came after the file's time.
					info, err := os.Stat(pendingPath)
					if err != nil {
						return err
					}
					if err := writeUnconfirmedReset(directory, unconfirmedReset{account.Key, pending.CreditID, info.ModTime().UTC()}); err != nil {
						return err
					}
				}
				return completeReset(ctx, directory, origin, client, pendingPath, pending, "it retries a reset whose reply was lost", &decision)
			}
			// The lost request never spent the credit, so the pool may no longer
			// need it: decide again from current allowance.
			if err := os.Remove(pendingPath); err != nil {
				return err
			}
			if unconfirmed != nil && unconfirmed.CreditID == pending.CreditID {
				if err := removeUnconfirmedReset(directory); err != nil {
					return err
				}
				unconfirmed = nil
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		readings, err := readCodexAccountsForReset(ctx, directory, origin, client)
		if err != nil {
			return err
		}
		if unconfirmed != nil {
			if waiting := unconfirmed.waitingFor(readings, time.Now()); waiting != "" {
				decision.Message = waiting
				return nil
			}
			if err := removeUnconfirmedReset(directory); err != nil {
				return err
			}
		}
		choice, found, reason := chooseResetCredit(readings, time.Now(), rules)
		if !found {
			decision.Message = reason
			return nil
		}
		pending := pendingReset{choice.account.Folder, choice.account.AccountID, choice.credit.ID, uuid.NewString()}
		data, err := json.Marshal(pending)
		if err != nil {
			return err
		}
		if err := writeAtomicData(pendingPath, data); err != nil {
			return err
		}
		if err := writeUnconfirmedReset(directory, unconfirmedReset{choice.account.Key, choice.credit.ID, time.Now().UTC()}); err != nil {
			return err
		}
		return completeReset(ctx, directory, origin, client, pendingPath, pending, choice.why, &decision)
	})
	return
}

// readCodexAccountsForReset reads every Codex account's credits and allowance
// from OpenAI.
func readCodexAccountsForReset(ctx context.Context, directory string, origin *url.URL, client *http.Client) ([]codexAccountReading, error) {
	pool, err := ReadPool(directory)
	if err != nil {
		return nil, err
	}
	var readings []codexAccountReading
	for _, account := range pool {
		if account.Provider != ProviderCodex {
			continue
		}
		credentials, err := newSavedLogin(filepath.Join(directory, account.Folder), client).Credentials(ctx)
		if requiresSignIn(err) {
			readings = append(readings, codexAccountReading{account: account, signedOut: true})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: cannot check credentials: %w", account.DisplayName, err)
		}
		var list struct {
			Credits []ResetCredit `json:"credits"`
		}
		if err := resetRequest(ctx, client, origin, credentials, http.MethodGet, "/backend-api/wham/rate-limit-reset-credits", nil, &list); err != nil {
			return nil, fmt.Errorf("%s: cannot check reset credits: %w", account.DisplayName, err)
		}
		var usage usageResponse
		if err := resetRequest(ctx, client, origin, credentials, http.MethodGet, "/backend-api/wham/usage", nil, &usage); err != nil {
			return nil, fmt.Errorf("%s: cannot check allowance: %w", account.DisplayName, err)
		}
		sample, err := usage.Sample(credentials.AccountID, time.Now())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", account.DisplayName, err)
		}
		readings = append(readings, codexAccountReading{account: account, allowance: sample, credits: list.Credits})
	}
	return readings, nil
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
func pendingCreditUnspent(ctx context.Context, origin *url.URL, client *http.Client, credentials Credentials, pending pendingReset) (bool, error) {
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

// completeReset sends the pending request and records its outcome. A spend
// OpenAI confirms stays unconfirmed until its reading shows it; a credit it
// did not spend is no longer waited for.
func completeReset(ctx context.Context, directory string, origin *url.URL, client *http.Client, path string, pending pendingReset, why string, decision *ResetDecision) error {
	account, credentials, err := pendingResetAccount(ctx, directory, client, pending)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"credit_id": pending.CreditID, "redeem_request_id": pending.requestID()})
	var result struct {
		Code string `json:"code"`
	}
	if err := resetRequest(ctx, client, origin, credentials, http.MethodPost, "/backend-api/wham/rate-limit-reset-credits/consume", body, &result); err != nil {
		return fmt.Errorf("reset outcome unknown; run the command again to retry the same credit: %w", err)
	}
	outcomes := map[string]string{
		"reset":            "spent",
		"already_redeemed": "spent, by an earlier try of this request",
		"nothing_to_reset": "not spent, because OpenAI found nothing to reset",
		"no_credit":        "not spent, because the credit is no longer available",
	}
	outcome, known := outcomes[result.Code]
	if !known {
		return fmt.Errorf("reset outcome unknown; run the command again to retry the same credit (provider result %q)", result.Code)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	decision.Spent = result.Code == "reset" || result.Code == "already_redeemed"
	if !decision.Spent {
		if err := removeUnconfirmedReset(directory); err != nil {
			return err
		}
	}
	decision.Message = fmt.Sprintf("Reset credit %s on %s %s. Chosen because %s.", pending.CreditID, account.DisplayName, outcome, why)
	return nil
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
