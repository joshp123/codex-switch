package accounts

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

// ObserveClaudeResponse retains all reported windows, including a model bucket
// absent from the next side request. Account state uses shared windows only.
func (monitor *Monitor) ObserveClaudeResponse(name store.AccountName, model string, headers http.Header, status int) {
	now := time.Now()
	monitor.mutex.Lock()
	monitored := monitor.monitoredAccountLocked(name, ProviderClaude)
	fact := monitored.fact
	fact.Provider = ProviderClaude
	incoming := AllowanceSample{ObservedAt: now}
	for header := range headers {
		header = strings.ToLower(header)
		claim := strings.TrimSuffix(strings.TrimPrefix(header, "anthropic-ratelimit-unified-"), "-utilization")
		if !strings.HasPrefix(header, "anthropic-ratelimit-unified-") || !strings.HasSuffix(header, "-utilization") || claim == "overage" {
			continue
		}
		prefix := "anthropic-ratelimit-unified-" + claim
		used, err := strconv.ParseFloat(headers.Get(prefix+"-utilization"), 64)
		reset := claudeHeaderReset(headers.Get(prefix + "-reset"))
		if err != nil || reset.IsZero() {
			continue
		}
		window := ReportedWindow{UsedPercent: used * 100, ResetsAt: reset, DurationSeconds: 7 * 86400, ObservedAt: now,
			LimitReached: claudeStatusLimited(headers.Get(prefix + "-status"))}
		switch claim {
		case "5h":
			window.DurationSeconds = 5 * 3600
		case "7d":
		default:
			if !strings.HasPrefix(claim, "7d") || model == "" {
				continue
			}
			window.ModelName = claudeModelDisplayName(model)
			switch strings.ReplaceAll(claim, "-", "_") {
			case "7d_opus":
				window.ModelName = "Opus"
			case "7d_sonnet":
				window.ModelName = "Sonnet"
			case "7d_fable", "7d_oi":
				window.ModelName = "Fable"
			}
			for _, known := range fact.AllowanceSample.Windows {
				matchesScope := ClaudeModelFamily(known.ModelName) == ClaudeModelFamily(window.ModelName)
				if known.ModelID != "" {
					matchesScope = matchesScope && claudeWindowMatchesModel(known, model)
				}
				if matchesScope && known.ResetsAt.Equal(reset) {
					window.ModelID, window.ModelName = known.ModelID, known.ModelName
					break
				}
			}
		}
		if validateClaudeWindow(window) == nil {
			incoming.Windows = append(incoming.Windows, window)
		}
	}
	monitor.recordWeeklyAllowance(string(name), incoming)
	fact.AllowanceSample = mergeClaudeAllowance(fact.AllowanceSample, incoming)
	fact.ClaudeRestrictions = mergeClaudeRestrictions(nil, fact.ClaudeRestrictions)
	if status == http.StatusTooManyRequests {
		restrictClaudeAccountAfterRejection(&fact, model, headers, incoming, now)
	} else if status >= 200 && status < 300 {
		clearClaudeRestrictionsFromSample(&fact, incoming)
	}
	updateClaudeSharedState(&fact, now)
	availabilityChange := describeAccountAvailabilityChange(monitored.fact, fact, "")
	monitored.fact = fact
	monitor.mutex.Unlock()
	if availabilityChange != "" {
		log.Print(availabilityChange)
	}
	if err := monitor.saveClaudeAllowanceCache(string(name), claudeAllowanceCache{Sample: fact.AllowanceSample, Restrictions: fact.ClaudeRestrictions}); err != nil {
		log.Printf("%s: could not save Claude limits: %v", fact.DisplayName, err)
	}
}

// restrictClaudeAccountAfterRejection keeps an account from model work after a
// 429. A usage limit is one Anthropic names: a window at its limit, or a
// representative claim. A 429 that names neither is a throttle: the account
// waits for Retry-After, or a minute, and is then tried again. Its
// unified-reset header is not when the account can work again. On 31 October
// it was midnight UTC, neither account's five-hour nor weekly reset, and
// treating it as a deadline kept two usable accounts from Opus for hours.
func restrictClaudeAccountAfterRejection(fact *AccountRoutingFact, model string, headers http.Header, incoming AllowanceSample, now time.Time) {
	scopes := make(map[string]time.Time)
	for _, window := range incoming.Windows {
		if !window.LimitReached && window.UsedPercent < 100 {
			continue
		}
		scope := claudeWindowRestrictionScope(window)
		if window.ResetsAt.After(scopes[scope]) {
			scopes[scope] = window.ResetsAt
		}
	}
	claim := headers.Get("anthropic-ratelimit-unified-representative-claim")
	claimedScope, claimNamesLimit := "", claim == "five_hour" || claim == "seven_day"
	if strings.HasPrefix(claim, "seven_day_") {
		claimedScope, claimNamesLimit = ClaudeModelFamily(model), true
	}
	limitReset := claudeHeaderReset(headers.Get("anthropic-ratelimit-unified-reset"))
	if claimNamesLimit && limitReset.After(scopes[claimedScope]) {
		scopes[claimedScope] = limitReset
	}
	retryAt := claudeRetryAfter(headers, now)
	if len(scopes) == 0 {
		if !retryAt.After(now) {
			retryAt = now.Add(time.Minute)
		}
		fact.ClaudeRestrictions[canonicalClaudeModelID(model)] = ClaudeRestriction{ResetsAt: retryAt, ObservedAt: now, Throttle: true}
		log.Printf("%s: Anthropic throttled %s without naming a usage limit; trying the account again after %s", fact.DisplayName, model, retryAt.Local().Format("15:04:05"))
		return
	}
	latestReset := retryAt
	for scope, reset := range scopes {
		if retryAt.After(reset) {
			reset = retryAt
		}
		fact.ClaudeRestrictions[scope] = ClaudeRestriction{ResetsAt: reset, ObservedAt: now, Claim: claim}
		if reset.After(latestReset) {
			latestReset = reset
		}
	}
	log.Printf("%s: Claude limit for %s; available after %s", fact.DisplayName, model, latestReset.Local().Format("Mon 2 Jan 15:04"))
}

func claudeModelDisplayName(model string) string {
	family := ClaudeModelFamily(model)
	if family == "" {
		return model
	}
	return strings.ToUpper(family[:1]) + family[1:]
}

func claudeStatusLimited(status string) bool { return status == "rejected" || status == "rate_limited" }

func claudeHeaderReset(value string) time.Time {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Unix(seconds, 0)
	}
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}

func claudeRetryAfter(headers http.Header, now time.Time) time.Time {
	if seconds, err := strconv.ParseFloat(headers.Get("Retry-After"), 64); err == nil && seconds > 0 {
		return now.Add(time.Duration(seconds * float64(time.Second)))
	}
	parsed, _ := http.ParseTime(headers.Get("Retry-After"))
	return parsed
}
