package accounts

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"
)

// automaticResetsFilename marks the one installation whose proxy spends reset
// credits by itself. A spend cannot be undone, and proxies on several Macs
// share the same accounts' credits: two that each decide from a usage reading
// that lags behind a spend would each spend one. So exactly one installation
// spends automatically, the one on a Mac that is always on. When that Mac is
// off, no credit is spent automatically and work waits for an account to
// refill. `codex-switch reset codex` spends on any Mac.
const automaticResetsFilename = "automatic-resets"

// AutomaticResetsOn reports whether this installation's proxy spends reset
// credits by itself. They are off unless turned on.
func AutomaticResetsOn(directory string) (bool, error) {
	_, err := os.Stat(filepath.Join(directory, automaticResetsFilename))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// SetAutomaticResets turns automatic resets on or off for this installation.
// The running proxy follows within one account read.
func SetAutomaticResets(directory string, on bool) error {
	path := filepath.Join(directory, automaticResetsFilename)
	if on {
		return writeAtomicData(path, []byte("on\n"))
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// codexWorkWaitingLifetime is how long a report of waiting Codex work counts.
// Waiting work asks for an account every five seconds, so a report this old
// means the work has gone.
const codexWorkWaitingLifetime = 15 * time.Second

// ReportCodexWorkWaiting tells the monitor that Codex model work is waiting
// because routing found no account for it.
func (monitor *Monitor) ReportCodexWorkWaiting() {
	monitor.codexWorkWaitingAt.Store(time.Now().UnixNano())
	select {
	case monitor.resetCheckRequested <- struct{}{}:
	default:
	}
}

// RunAutomaticResets spends a reset credit by the rule in chooseResetCredit
// until ctx ends, while automatic resets are on. It looks once per account
// read, and at once when work starts waiting. It asks OpenAI only when the
// latest readings show a need: work waiting for an account, or a credit in its
// last hour. The decision itself is made from a fresh read of every account.
func (monitor *Monitor) RunAutomaticResets(ctx context.Context) {
	ticker := time.NewTicker(monitor.readInterval)
	defer ticker.Stop()
	lastMessage, lastSwitch := "", ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-monitor.resetCheckRequested:
		}
		now := time.Now()
		workWaiting := now.Sub(time.Unix(0, monitor.codexWorkWaitingAt.Load())) < codexWorkWaitingLifetime
		on, err := AutomaticResetsOn(monitor.dataDirectory)
		switched := "Automatic Codex resets are off: this proxy spends no reset credit by itself."
		if on {
			switched = "Automatic Codex resets are on: this proxy spends a reset credit when the Codex pool is out, or before a credit expires."
		}
		if err != nil {
			switched = "Automatic Codex resets are off: " + err.Error()
		}
		if switched != lastSwitch && ctx.Err() == nil {
			log.Print(switched)
			lastSwitch = switched
		}
		var decision ResetDecision
		message := ""
		switch rules := (resetRules{whenPoolOut: workWaiting, beforeExpiry: monitor.creditMayExpireWithinTheHour(now)}); {
		case !on:
			if workWaiting {
				message = "Codex work is waiting for an account. Automatic Codex resets are off, so no reset credit is spent for it."
			}
		case rules.whenPoolOut || rules.beforeExpiry:
			decision, err = spendResetCredit(ctx, monitor.dataDirectory, monitor.codexOrigin, monitor.httpClient, rules)
			message = decision.Message
			if err != nil {
				message = "Automatic reset: " + err.Error()
			}
		}
		// A decision that has not changed is logged once.
		if message != "" && (message != lastMessage || decision.Spent) && ctx.Err() == nil {
			log.Print(message)
		}
		lastMessage = message
		if decision.Spent {
			monitor.RequestAccountRoutingFactsRefresh()
		}
	}
}

// creditMayExpireWithinTheHour reads the monitor's latest credit lists, which
// are up to five minutes old.
func (monitor *Monitor) creditMayExpireWithinTheHour(now time.Time) bool {
	monitor.mutex.Lock()
	defer monitor.mutex.Unlock()
	for _, monitored := range monitor.accounts {
		if monitored.fact.Provider != ProviderCodex {
			continue
		}
		for _, credit := range monitored.fact.ResetCredits {
			if credit.usable(now) && !credit.ExpiresAt.After(now.Add(expiringCreditWindow)) {
				return true
			}
		}
	}
	return false
}

// ResetCreditComesFirst reports whether a reset credit is about to be spent,
// so no account may draw its usage credits yet: reset credits expire sooner.
// That is when a spend from this installation awaits its outcome or OpenAI's
// reading of it, or when automatic resets are on here and the latest readings
// show the pool out with a credit to spend; see choosePoolOutCredit. It reads
// no network, so routing can ask it.
func (monitor *Monitor) ResetCreditComesFirst() bool {
	if _, err := os.Stat(filepath.Join(monitor.dataDirectory, "pending-reset.json")); err == nil {
		return true
	}
	now := time.Now()
	readings := monitor.latestCodexReadings()
	if unconfirmed, err := readUnconfirmedReset(monitor.dataDirectory); err != nil || (unconfirmed != nil && unconfirmed.waitingFor(readings, now) != "") {
		return true
	}
	if on, err := AutomaticResetsOn(monitor.dataDirectory); err != nil || !on {
		return false
	}
	if _, refused := poolResetRefusedAt(monitor.dataDirectory, now); refused {
		return false
	}
	_, found, _ := choosePoolOutCredit(readings, now)
	return found
}

// latestCodexReadings is every Codex account as the monitor last read it.
func (monitor *Monitor) latestCodexReadings() []codexAccountReading {
	pool, err := ReadPool(monitor.dataDirectory)
	if err != nil {
		return nil
	}
	monitor.mutex.Lock()
	defer monitor.mutex.Unlock()
	var readings []codexAccountReading
	for _, account := range pool {
		monitored, known := monitor.accounts[account.Key]
		if account.Provider != ProviderCodex || !known {
			continue
		}
		fact := monitored.fact
		readings = append(readings, codexAccountReading{account: account, signedOut: fact.AccountState == AccountSignInRequired,
			allowance: fact.AllowanceSample, credits: fact.ResetCredits})
	}
	return readings
}
