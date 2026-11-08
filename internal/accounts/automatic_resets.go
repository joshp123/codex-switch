package accounts

import (
	"context"
	"log"
	"time"
)

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
// until ctx ends. It looks once per account read, and at once when work starts
// waiting. It asks OpenAI only when the latest readings show a need: work
// waiting for an account, or a credit in its last hour. The decision itself is
// made from a fresh read of every account.
func (monitor *Monitor) RunAutomaticResets(ctx context.Context) {
	ticker := time.NewTicker(monitor.readInterval)
	defer ticker.Stop()
	lastMessage := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-monitor.resetCheckRequested:
		}
		now := time.Now()
		rules := resetRules{
			whenPoolOut:  now.Sub(time.Unix(0, monitor.codexWorkWaitingAt.Load())) < codexWorkWaitingLifetime,
			beforeExpiry: monitor.creditMayExpireWithinTheHour(now),
		}
		if !rules.whenPoolOut && !rules.beforeExpiry {
			lastMessage = ""
			continue
		}
		decision, err := spendResetCredit(ctx, monitor.dataDirectory, monitor.codexOrigin, monitor.httpClient, rules)
		message := decision.Message
		if err != nil {
			message = "Automatic reset: " + err.Error()
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
